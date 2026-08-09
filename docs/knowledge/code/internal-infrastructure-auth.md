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
authentication on the pod for as long as the connection hangs. Every refusal returns
one sentinel, `ErrUnauthenticated`, mapped to one message — a specific one only tells the
sender which part of the token to fix next. The reason is wrapped so the service can
**log** it, never return it.

## Empty config defaults; a wrong one fails the pod

`New` substitutes `constants.DefaultJWKSURL`/`DefaultAudience`/`DefaultIssuer` for empty
fields — what `LoadConfig` supplies and what the chart injects. Erroring on empty instead
turns every path that builds a `Config` by hand into a service refusing all traffic. A
JWKS URL that is *present and unusable* — including an absolute one whose scheme
`http.Client` cannot fetch — still fails `New` and stops the pod: a degraded
verifier has two behaviours and both are wrong — refusing everything is a confusing
outage, allowing everything is the hole this package closes.
`JWT_AUTH_DISABLED_MOCK_LOCAL_PRINCIPAL` bypasses all of it and returns a fixed actor for
any token; the name is deliberately unpleasant, the container logs a `WARN` on every boot
that sets it, and `VerifyActor` returns a **copy**, since a handler mutating the mock in
place would otherwise rewrite the identity of every later request.

That bypass is **refused, not warned about, inside Kubernetes.** The chart declares the key
under `app.environment` and `deployment.yaml` renders any override, so an unpleasant name
and a boot warning could not stop a deploy from switching authentication off; `Config.
InCluster` — set from `KUBERNETES_SERVICE_HOST`, which the kubelet injects and the chart
cannot unset — makes `New` return an error naming the key instead. Erroring rather than
quietly verifying for real is the point: a values file asking for no authentication has to
be fixed, not tolerated by whichever build happens to carry the guard.
