# 2026-08-24 — LFXV2-3033 a cached token survives its own 401

**Fix** — client caching turned a one-call auth failure into a stuck client, on all three
providers that mint their own token.

Copilot reported it on two lines: `internal/dispatch/microsoft.go:502` and
`internal/dispatch/reddit.go:291`. Both reproduced by construction — two operations on one
client against a token endpoint handing out a distinct token per mint, and an API returning
401. The token endpoint was hit **once**, and the second request re-presented `tok_1`, the
token the platform had already rejected.

The 401-handling code is older than this PR, but the PR is what makes it reachable: a client
rebuilt per operation started with an empty cache, so a revoked token cost one failure and the
next operation re-minted. A client cached across operations serves the same rejected token
until its ADVERTISED expiry — which for a revoked token never usefully arrives. On a dispatch
path that spends money that is a stuck campaign, not a transient error.

**Google Ads was the third, and nobody reported it.** `internal/dispatch/googleads.go` resolves
through the same `clientCache`, and its token cache is the same design. The same test fails
there identically. Fixing only the two reported lines would have left the class open.

LinkedIn, Meta and X are NOT in the class, and the reason matters: they carry a stored
credential rather than minting one, so there is no cached token to invalidate. The class is
exactly the three OAuth-refresh clients — the sweep is over, not merely wider.

## Where the guard goes

Each fix is ONE call at the point that dominates every 401-bearing exit, not one call per exit:

- **reddit** — the single non-2xx return in `request`.
- **microsoft** — `attempt`'s status check, PLUS `statusAwareReadError`. The second is not
  redundant: the unreadable-body and oversize-body arms `return` from `attempt` *before* the
  status check, so a guard written only at the obvious site leaves both re-presenting the
  rejected token.
- **googleads** — placed immediately after the 429 branch, because three separate exits below
  it (unreadable body, oversize body, parsed body) each build a 401-bearing `apiError`.

The guard is on the STATUS, never on the parsed body. An unreadable or unparseable 401 is
still a 401, and it is the case likeliest to accompany a broken auth response — a
body-dependent guard would go quiet exactly when it is needed. `TestDoRequest_UnreadableBody401AlsoInvalidates`
pins that arm separately, and it is the test that dies when only the `statusAwareReadError`
call is removed while the readable-401 test stays green.

Each invalidator clears the EXPIRY as well as the token, so the cache reads empty by either
half of the fast-path condition and a later edit to that test cannot resurrect the token. An
in-flight single-flight refresh is deliberately left alone: it is already fetching a new token.

## Mutation

Neutering all three `invalidateAccessToken` bodies to `{}` compiles, and kills all five tests
(3 readable-401, 2 unreadable-401). Removing only microsoft's `statusAwareReadError` call kills
the unreadable test alone. No survivors.
