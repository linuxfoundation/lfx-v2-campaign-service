---
type: "Code Concept"
title: "internal/infrastructure/auth"
description: "Verifies the Heimdall-issued bearer token against Heimdall's JWKS and turns its claims into the domain actor recorded on writes."
resource: "internal/infrastructure/auth"
---

# internal/infrastructure/auth

## Why verify a token the gateway already verified

Heimdall validates every route — the chart's parity test enforces that none escapes it —
so on the happy path this proves nothing new. It exists because **the gateway's guarantee
stops at the cluster boundary**: anything reaching the pod directly (a misconfigured
NetworkPolicy, a sibling workload, a `kubectl port-forward`) never passes Heimdall at all.
What makes that worth closing here rather than filing as a network concern is where the
claims go: the principal is written to `created_by`/`updated_by`, so an unverified claim
was a **forgeable audit trail** for who authorized paid ad spend.

## What is checked

The verifier is `github.com/auth0/go-jwt-middleware/v2`, matching `lfx-v2-query-service`
and `lfx-v2-meeting-service` (both at `internal/infrastructure/auth/jwt.go`).

| Check | Value | Why it is not optional |
|---|---|---|
| Algorithm | `PS256`, **pinned** | Reading `alg` from the header is what makes `none` and HS256-against-the-public-key work. |
| Signature | Heimdall's JWKS, cached 5 min | The refetch on expiry lets a key rotation take effect without restarting the pod. |
| Issuer | `heimdall` | An issuer whose key set an attacker controls could otherwise mint valid tokens. |
| Audience | `lfx-v2-campaign-service` | A token minted for a sibling service would otherwise authorize spend here. |
| `exp` / `nbf` | 5s skew, `exp` **required** | A leaked token has to stop working — and the library validates `exp` only when the claim is present, so a signed token omitting it would never expire. |
| `principal` | non-empty after trim | A *verified* token attributing a write to nobody carries the authority of having been checked. |

`jwks.WithCustomJWKSURI` is required: the issuer is the bare name `heimdall`, not a URL, so
the provider cannot derive the key-set address by OIDC discovery. The fetch gets an
explicit client timeout: the provider's default `http.Client` has none, and the cold fetch
runs while holding the provider's write lock, so a stalled Heimdall would block every
authentication on the pod for as long as the connection hangs.

That write lock is also why the key fetch is **coalesced**. `jwks.CachingProvider` does not
do it itself: on a miss, `refreshKey` takes the lock and fetches *without re-checking the
cache*, so N simultaneous first requests become N serialized fetches and the Nth caller
waits roughly N × fetch. Authentication runs on every request, so the queue is the whole
request load.

The reachable cases are narrower than "any cache miss", and the wider claim is worth
refusing explicitly because it is the one that suggests itself. An ordinary **TTL expiry
does not** stampede: `CachingProvider` holds a semaphore, admits exactly one background
refresher, and serves the STALE key set to everyone else while that one runs, so no caller
blocks and no second fetch starts. Two things do reach the cold path — **startup**, before
anything is cached, and the second-order case where that lone background refresh **fails**
and deletes the stale entry it was serving from. The next N callers then find an empty cache
and serialize, which means the stampede arrives precisely when the JWKS endpoint is already
unhealthy. `coalesceKeyFunc` wraps the provider's `KeyFunc` in a `singleflight` group,
using `DoChan` so each caller keeps its own deadline, and `context.WithoutCancel` inside the
shared call so the first caller's cancellation cannot fail everyone waiting on its result.

Every refusal **of the token** returns one sentinel, `ErrUnauthenticated`, mapped to one
message — a specific one only tells the sender which part of the token to fix next. The
reason is wrapped so the service can **log** it, never return it.

A key-func failure is *not* one of those refusals, and it arrives inside the same error.
`validator.ValidateToken` wraps whatever the key func returns (`%w`, twice over), so a
JWKS fetch that failed, timed out, or was cancelled reached `VerifyActor` indistinguishable
from a bad signature — and was reported as `ErrUnauthenticated`, i.e. HTTP 400 "invalid
bearer token", to a caller whose credential was perfectly good. That is reachable on a cold
cache and at every TTL expiry, and 400 additionally tells the caller not to retry a
condition that clears by itself. `coalesceKeyFunc` therefore tags **both** arms of its
select with `ErrKeyUnavailable` (a re-export of `domain.ErrKeyUnavailable`; it is the only
place that can tag it), and `VerifyActor` passes it through untouched instead of wrapping.
The cancellation arm is tagged too: that our wait ended rather than Heimdall's answer
arriving still leaves nothing established about the token. The service layer maps the
sentinel to **503** and every token-side refusal to 400.

## A JWKS fetch that "succeeds" with no keys

The 400-for-our-outage bug above has a second, quieter source that tagging cannot reach,
because the fetch does not fail. go-jwt-middleware v2.3.1 never checks the HTTP status:
`jwks.Provider.KeyFunc` goes straight from `Client.Do` to
`json.NewDecoder(response.Body).Decode(&jwks)` (`jwks/provider.go:84-93`). A
`jose.JSONWebKeySet` has one field and ignores every other, so an error object — a 404
`{"error":"not found"}` from a mistyped path, a 502 from a proxy in front of Heimdall —
decodes **cleanly** into a key set with zero keys and is returned as a success.
`CachingProvider` then stores it for the full five-minute TTL, and for those five minutes
every caller holding a good token is told their credential is bad.

Both checks live in **`jwksStatusGuard`**, a `RoundTripper` that runs *before* the
provider decodes anything:

- A **non-2xx** becomes a transport error. It drains a bounded prefix of the body
  (`jwksErrorBodyPeek`) **to `io.Discard`** and closes it, which keeps the connection
  poolable on an endpoint that fails on every refresh. The body reaches neither the error
  nor the log.
- A **2xx** is read in full (bounded by `jwksMaxBody`), its `keys` array counted, and the
  bytes replayed on the response so the provider can still decode them. Zero keys is an
  error, whatever produced it: a 200 carrying `{"keys":[]}`, an issuer mid-rotation with
  nothing published, a future middleware version that drops the status check some other
  way. So is a body that will not decode as an object with a `keys` array at all. The
  size bound is enforced as a *refusal*, not a truncation — a truncated key set decodes
  to fewer keys, which is the empty-set failure in slow motion.

**Why both are at the transport and not around `KeyFunc`.** The empty-set check used to
be a `requireKeys` wrapper around `provider.KeyFunc`, and that placement was wrong for a
reason worth carrying past this file: `CachingProvider.refreshKey` **stores the decoded
value before returning it** (`jwks/provider.go:207-221`). A wrapper therefore rejects the
empty set only after it is already cached, so every request answers 503 for the full
five-minute TTL even though Heimdall recovered seconds later. **A validity check outside
a caching layer cannot keep an invalid value out of the cache** — it has to run on the
side of the cache the value arrives from. Failing inside the `RoundTripper` makes the
fetch an *error*, and `refreshKey` caches only successes.

The move also removed this package's `go-jose` import, which closes a second hazard: the
old wrapper type-asserted against `gopkg.in/go-jose/go-jose.v2`, the version the
middleware decodes into (`jwks/provider.go:13`), and **a type assertion against a
dependency's type is only a check if it is the same major version the dependency
returns** — assert a different major and it compiles, vets and lints clean, never
matches, and silently passes everything through. Counting `json.RawMessage` entries off
the raw body has no version to get wrong.

`TestVerifyActor_EmptyKeySetIsUnavailable` serves `{"keys":[]}`, then flips the handler
to a real key set and asserts the **next** call succeeds; that recovery assertion is what
pins the placement — move the check back up and it hangs on the TTL instead of passing.
`TestVerifyActor_JWKSErrorBodyIsNotMistakenForAKeySet` covers the 404 case and asserts
the HTTP status reaches the operator: the empty-set arm catches that case too, but can
only say "no signing keys", which describes a healthy issuer mid-rotation just as well as
it describes a wrong URL. `TestVerifyActor_UndecodableSuccessBodyIsUnavailable` and
`TestVerifyActor_OversizeKeySetIsRefused` cover the other two 2xx shapes.

## Redirects are followed; credentials are not

Everything that is not 2xx used to become an error, and a 3xx is not a failure. **An
`http.RoundTripper` sits below `http.Client`'s redirect handling**: the Client only follows a
3xx it is handed, so returning an error means it never sees one and never follows. An ordinary
http→https upgrade or a CDN hop then becomes a permanent `ErrKeyUnavailable` — every refresh
takes the same path. A 3xx is passed straight back instead.

Following is safe because the credential does not travel with it. `credentialed` dresses the
**first hop only**, matching `req.URL` against the sanitized URL handed to the provider. Two
things depend on that gate:

- `RoundTrip` runs once per hop, so a guard that swapped unconditionally would rewrite every
  hop's URL back to the configured endpoint — an immediate loop to the Client's redirect limit,
  re-sending the credential each time.
- The operator's credentials belong to the host they configured. `net/http` drops
  `Authorization` across hosts on its own; the gate additionally withholds it (and the
  operator's query, which `net/http` does not drop) from a same-host redirect to a different
  path.

The chain is bounded by the Client's own 10-hop limit, and whatever finally answers 2xx still
comes back through `RoundTrip` and is gated by `checkKeys`.

`TestVerifyActor_FollowsAJWKSRedirectWithoutForwardingCredentials` pins both halves — each
alone is satisfied by a broken version. It asserts the configured endpoint *does* receive the
credentials, the redirect target is hit exactly once, resolution succeeds, and the target sees
neither an `Authorization` header nor the operator's query. Reverting the 3xx pass-through
fails it with `returned HTTP 302`; removing the first-hop gate fails it with
`stopped after 10 redirects`.

## Empty config defaults; a wrong one fails the pod

`New` substitutes `constants.DefaultJWKSURL`/`DefaultAudience`/`DefaultIssuer` for empty
fields — what `LoadConfig` supplies and what the chart injects. Erroring on empty instead
turns every path that builds a `Config` by hand into a service refusing all traffic. A
JWKS URL that is *present and unusable* — including an absolute one whose scheme
`http.Client` cannot fetch — still fails `New` and stops the pod: a degraded
verifier has two behaviours and both are wrong — refusing everything is a confusing
outage, allowing everything is the hole this package closes.
**Neither refusal renders the configured URL.** A URL can carry credentials in its
userinfo, and `main.go` logs whatever `New` returns, so a rejected value printed verbatim
writes a password into the startup log of every pod that fails to boot — where it survives
rotation of the thing it protected. The scheme check prints `redact.URL(jwksURL)`; the parse
failure is *reported, not wrapped*, because `url.Parse`'s own error embeds the whole raw URL
of its own accord — wrapping it leaks by inheritance rather than by formatting. Both name
`JWKS_URL`, the config key, which is the part an operator needs.
`TestNew_RejectionDoesNotLeakURLCredentials` covers both arms; the second exists because it
is the one that looks safe.
**The printable form is [`pkg/redact`](pkg-redact.md), not `url.URL.Redacted()`.** `Redacted()`
masks the password and KEEPS the username, and this service treats a username in a
credential-bearing URL as credential material — a basic-auth gateway issues the pair together,
so printing half of it narrows an attacker's search. The same helper is used at every
`jwksStatusGuard` formatting site, which matters more than the startup one: the startup URL is
rendered once, while the guard renders `req.URL` on every failed refresh, so a misconfigured
endpoint writes the same line on a loop.
`TestJWKSStatusGuard_ErrorsDoNotLeakURLCredentials` covers all four fetch-error arms
separately, because the leak is per-format-verb — fixing one and missing another leaves the
credential in the logs just as often.

**Redacting every string this package formats is still not enough, because one of the
strings is not ours.** `http.Client.Do` wraps *every* transport failure — refused
connection, DNS miss, timeout — in a `*url.Error` it builds from `req.URL`, and
`net/url`'s own masking replaces the **password** while keeping the **username and the
entire query** verbatim. A JWKS URL of the operator's shape
`https://svc:pw@gw/jwks?access_token=…` therefore renders as
`Get "https://svc:***@gw/jwks?access_token=s3cret"`, and `internal/service/auth.go` logs
that whole chain on the `ErrKeyUnavailable` path. No amount of care at our own format
sites reaches it.

So the credential is removed from the URL `http.Client` ever sees. `New` hands
`jwks.WithCustomJWKSURI` a copy with `User`, `RawQuery` and the fragment cleared, and
`jwksStatusGuard` holds the operator's real URL in `outbound`; `credentialed` clones each
outgoing request and swaps it back in. **The basic credential is re-applied as an explicit
`Authorization` header, not left on the URL**, because the thing that turns URL userinfo
into that header is `http.Client.send`, which runs *before* the transport — a URL repaired
inside `RoundTrip` would authenticate nothing. Fixing it here rather than by sanitizing the
error `KeyFunc` returns is what makes it cover every failure mode at once, with no
dependence on the error chain's shape.

The two tests are complements and each is binding against a different degenerate version.
`TestVerifyActor_JWKSURLCredentialsNeverReachTheError` goes through `VerifyActor` — not the
guard's `RoundTrip`, which never sees the wrapper — against a closed port, and walks every
`errors.Unwrap` layer for `svc`, `pw`, `s3cret` and `access_token` while still requiring the
host to be named. `TestVerifyActor_JWKSURLCredentialsStillAuthenticate` serves the key set
only to a request carrying **both** credentials, so the leak cannot be "fixed" by quietly
dropping them: a strip without the swap turns the fetch into a 401, which is itself an
`ErrKeyUnavailable` and satisfies every assertion in the first test.

The upstream **response body** is a second channel, and it was open for a while behind a
comment that argued it was closed. Both guard paths logged a bounded prefix of the body at
debug, reasoning that bounding the length and naming the origin made it safe. That conceded
the premise — the body is untrusted upstream text — and then ignored it. Length is not the
property that matters: an error page can reflect the request back, and a gateway that
rejected our `Authorization` header is the case most likely to quote it, which is also
exactly when an operator turns debug on. Nor is the debug **level** a mitigation; debug
output lands in the same log store. Both paths now log the redacted endpoint and the status
only, and `TestJWKSStatusGuard_UpstreamBodiesNeverReachTheLog` captures the handler output
and fails on both the secret and on the reappearance of a `body_prefix` attribute — while
still asserting the endpoint survives, so the test cannot be satisfied by logging nothing.

`JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` bypasses all of it and returns a fixed actor for
any token; the name is deliberately unpleasant, the container logs a `WARN` on every boot
that sets it, and `VerifyActor` returns a **copy**, since a handler mutating the mock in
place would otherwise rewrite the identity of every later request.

That bypass is **refused, not warned about, inside Kubernetes.** The chart declares the key
under `app.environment` and `deployment.yaml` renders any override, so an unpleasant name
and a boot warning could not stop a deploy from switching authentication off; `Config.
InCluster` makes `New` return an error naming the key instead. Erroring rather than
quietly verifying for real is the point: a values file asking for no authentication has to
be fixed, not tolerated by whichever build happens to carry the guard.

**`InCluster` is deliberately not read from `KUBERNETES_SERVICE_HOST` alone.** An earlier
version was, on the reasoning that the kubelet injects it and the chart cannot unset it.
The second half of that was false: `deployment.yaml` renders every key of
`app.environment` and appends `app.extraEnv` verbatim, and an explicit container `env`
entry takes precedence over the kubelet's service variables — so one override could set
the mock principal *and* declare `KUBERNETES_SERVICE_HOST: ""`, clearing the very
discriminator meant to catch it. Exactly the combination the guard exists to prevent.

It is closed at both layers, because each covers what the other cannot:

- **Template time** — a guard at the top of `deployment.yaml` `fail`s the render if either
  env input declares any `KUBERNETES_*` name, or gives the bypass key a non-empty value —
  **or a `valueFrom`.** Both env inputs support secret and field references, whose contents
  the template cannot read, so the whole form is refused for this one key rather than
  inspected: "cannot see it" must not read as "it is empty" for the variable that turns
  authentication off. `valueFrom` stays available for every other key, which is how
  credentials are normally injected.
  A deploy carrying the hole cannot be produced, so there is no window in which a cluster
  is running it. This does nothing for deploys that never pass through the chart.
- **Runtime** — `config.runningInCluster` ORs in a signal the environment cannot express:
  the projected service-account directory at
  `/var/run/secrets/kubernetes.io/serviceaccount`. A hand-applied manifest, a `kubectl
  patch`, or an ArgoCD override that clears the variable still meets a pod that knows it
  is in a cluster. Suppressing this one needs `automountServiceAccountToken: false` — a
  separate, visible change, not one more line in the same env block.

Both signals are consulted rather than just the file, so a pod legitimately running
without an automounted token is still recognized.
