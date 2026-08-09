# 2026-08-09 — The bearer token is verified, not decoded (LFXV2-3053)

**Update** — the bearer token is now verified against Heimdall's JWKS instead of being
base64-decoded and trusted.

**The gap.** `JWTAuth` split the token on `.`, base64-decoded the middle segment and
believed it — no signature, issuer, audience or expiry check. The config for doing better
was wired end to end (the chart injects `JWKS_URL` and `JWT_AUDIENCE`, `LoadConfig` reads
them into `JWKSUrl`/`Audience`/`Issuer`) and **nothing consumed them.** Heimdall validates
every route, so closing this only earns its keep when something reaches the pod without
passing the gateway — what settles it is not the probability of that but the consequence:
the principal is written to `created_by`/`updated_by`, so an unverified claim was a
forgeable audit trail for who authorized paid ad spend.

**What was built.** `internal/infrastructure/auth`, following
`lfx-v2-query-service`/`lfx-v2-meeting-service` rather than hand-rolling JWKS handling:
PS256 pinned, issuer `heimdall`, audience `lfx-v2-campaign-service`, 5-minute key cache,
5-second skew, non-empty `principal` required, one `ErrUnauthenticated` sentinel. In
`internal/service` one embedded `authGuard` serves all three services, and
`actorFromToken` — the decoder — is deleted.

**Three decisions that could have gone the other way.** *One PR, not "package first, wire
later"*: an unreferenced verification package passes every gate and secures nothing. *A
nil verifier rejects rather than falls back*: decoding-as-before would put the old
behaviour one wiring mistake away, so failing closed makes it an outage — and
`TestNewContainer_AllPathsInjectTheTokenVerifier` makes it a test failure first, three
services × every boot path, since the degraded paths construct theirs independently. *An
empty JWKS URL defaults, a wrong one fails the pod*: erroring on empty turned every
hand-built `config.Config` into a service refusing all traffic, while defaulting a *typo*
would hide a misconfiguration.

**Stale comments were the real hazard.** Two survived asserting the old model: the parity
test's `JWT_ISSUER` exemption ("empty means no issuer verification" — the check is now
unconditional and empty selects the default) and `attributedActor`'s doc.

**What this supersedes.** The 2026-08-07 entry (LFXV2-3038) accepted that *a missing actor
does not fail the write*, because rejecting would escalate a token-**decoding** regression
into a total outage of brief creation. That premise is gone: the token is now verified, so
what a NULL-attributed row records is an unauthenticated request. `JWTAuth` refuses before
any handler runs, making the nil branch unreachable through the served routes; the warning
it emits is kept as a tripwire for a future entry point wired without the security scheme.
That entry's own file is left untouched — one file per entry.

**Two library defaults that do not hold on their own.** `validator` checks `exp` only when
the claim is PRESENT, so a signed token omitting it would never expire — `VerifyActor`
rejects a zero expiry itself. And `jwks.NewCachingProvider` defaults to an `http.Client`
with no timeout while the cold fetch holds the provider's write lock, so the key fetch is
given a bounded client.

**The bypass that survived the rewrite.** Verifying the token is worth nothing if the
process can be told not to. `JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` disables verification
outright, and "local development only" was a comment, not a control: the chart declares the
key under `app.environment` and `deployment.yaml` renders whatever it holds, so a `--set`
or an edited values file could ship a running pod accepting ANY bearer token as that
principal — on the endpoints that spend money. The empty default and the parity exemption
keep the chart honest about *its own* value; neither can stop an override.

`New` now refuses the mock principal when `KUBERNETES_SERVICE_HOST` is set. That variable
is the discriminator for one reason: the kubelet injects it into every pod and the chart
cannot unset it, so the same override that would enable the bypass cannot also conceal the
cluster. A laptop, `go run`, a plain container and CI do not have it, so the workflow this
switch exists for is untouched.

The refusal is an **error, not a silent downgrade** to real verification. Starting anyway —
verifying, serving, saying nothing — would leave the request path safe and the intent live
in a values file, and the next deploy of a build without this guard would ship the hole with
nothing having ever complained. Fail the pod and name the key to unset.
`TestNew_MockPrincipalIsRefusedInCluster` pins both halves: refused inside a cluster,
still honoured outside one.

**Known follow-up.** Rejections surface as **400**, not 401 — the design declares no
Unauthorized type and `commonBriefErrors` documents 400 as the JWTAuth rejection status;
adding 401 is a design change with generated-code blast radius, filed separately.
