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

`New` now refuses the mock principal when the process detects a cluster. A laptop,
`go run`, a plain container and CI do not, so the workflow this switch exists for is
untouched.

**The first version of that detection was `KUBERNETES_SERVICE_HOST != ""`, and the reason
given for it was wrong.** The claim was that the kubelet injects the variable and the
chart cannot unset it, so the same override that enables the bypass cannot also conceal
the cluster. But `deployment.yaml` renders every key of `app.environment` and appends
`app.extraEnv` verbatim, and an explicit container `env` entry takes precedence over the
kubelet's service variables — so a single override could set the principal *and* declare
`KUBERNETES_SERVICE_HOST: ""`, producing a pod that accepts any bearer token as that
principal. The guard was defeated by exactly the input it was guarding.

Worth naming as a class: the property being relied on was "the chart cannot express this",
and that was never verified against the template. A rendering loop over an arbitrary map
can express *any* variable name, which makes "the environment says so" a weak foundation
for any security discriminator.

The replacement does not rest on one layer:

- **Template time** — a guard at the top of `deployment.yaml` `fail`s the render when
  either env input declares a `KUBERNETES_*` name or gives the bypass key a non-empty
  value. The deploy cannot be produced, so no cluster ever runs it.
- **Runtime** — `config.runningInCluster` ORs the variable with the presence of the
  projected service-account directory, a signal the environment cannot express. This
  covers what the chart guard structurally cannot: manifests applied outside Helm.

Whitespace is treated as unset in both places, matching `LoadConfig`'s `TrimSpace` — a
value the service ignores must not fail a deploy.

Each guard was revert-checked. Dropping the service-account signal fails
`TestRunningInCluster/service-account mount only, variable cleared` with `= false, want
true`; dropping the template guard makes `helm template --set
app.environment.KUBERNETES_SERVICE_HOST.value=` succeed, which
`TestDeploymentRejectsReservedAndBypassEnv` reports as rendering an env block that can
disable authentication.

The refusal is an **error, not a silent downgrade** to real verification. Starting anyway —
verifying, serving, saying nothing — would leave the request path safe and the intent live
in a values file, and the next deploy of a build without this guard would ship the hole with
nothing having ever complained. Fail the pod and name the key to unset.
`TestNew_MockPrincipalIsRefusedInCluster` pins both halves: refused inside a cluster,
still honoured outside one.

**And the guard was answering a question that was not its own.** `authenticate` refused an
empty token before consulting the verifier. For a deployed pod that is a no-op — the real
verifier rejects `""` explicitly — so the only verifier it ever overrode was the local mock,
whose entire contract is that every request is that principal. Goa passes `""` for an absent
`Authorization` header, and a developer running without Auth0 has no header to send, so the
mock worked only for callers who had invented a dummy token: the workflow it exists for was
the one case it did not serve. The check is gone; who may authenticate is the verifier's
question, and `TestAuthenticate_EmptyTokenIsTheVerifiersCallNotTheGuards` pins that a
verifier accepting `""` is honoured while the existing half keeps the ordinary rejection.

**Known follow-up.** Rejections surface as **400**, not 401 — the design declares no
Unauthorized type and `commonBriefErrors` documents 400 as the JWTAuth rejection status;
adding 401 is a design change with generated-code blast radius, filed separately.
