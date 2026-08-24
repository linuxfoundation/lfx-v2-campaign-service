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

## The first fix was racy: compare-and-clear, not clear

The first version cleared unconditionally, and Copilot re-reviewed it into a real ABA race on
all three providers. With a SHARED client the invalidator can be called about a token the cache
no longer holds: request A leaves carrying `tok_1`, request B refreshes and caches `tok_2`, and
A's late 401 then arrives naming `tok_1`. An unconditional clear evicts `tok_2` — a token
nothing rejected — and a burst of late responses drives serial re-exchanges, defeating the very
single-flight coalescing these clients already have.

Proved it before fixing: seeding the cache with `tok_current` and invoking the unconditional
invalidator emptied it, though no 401 had named that token.

So the invalidator now takes the PRESENTED token and clears only on a match. That makes it
idempotent and self-limiting — the rejected token is dropped exactly once, and every later 401
naming it is a no-op because the cache has moved on. The token is threaded through every 401
arm, which for microsoft meant widening `statusAwareReadError` to carry it (its two callers are
the unreadable and oversize arms, where the token is still in scope in `attempt`).

An empty `presented` never clears, so a caller that cannot name a token cannot flush the cache.

## The guard belongs on the status line, not after the body read

A later round found microsoft still reading the full response body BEFORE its 401 guard ran.
That is not merely untidy: a slow or truncated 401 leaves the rejected token readable from
`accessTokenValue` for the rest of the attempt timeout, and every concurrent caller on the
shared client keeps sending it. Reading first buys no accuracy — a 401 is a 401 whether or not
its body arrives.

Hoisting the guard to immediately after `Do` fixed it and SIMPLIFIED the change: the unreadable
and oversized arms are now covered upstream, so `statusAwareReadError` no longer needs the token
threaded into it at all and reverts to its original signature. google-ads already had this
shape.

Reddit had the SAME defect and I missed it on the first pass, because I fixed the site the
review named instead of the class it described. `readResponseBody` runs before the non-2xx arm
there too, and it can block until the per-attempt deadline. The next review round found it, and
the lesson is the cheaper one to write down than to relearn: when a finding is about WHERE a
guard sits, the sweep is every client with that guard, not the file in the comment. All three
now read `Do` -> invalidate-on-status -> body read, and reddit's late guard was deleted rather
than left as a harmless duplicate.

`TestAttempt_401InvalidatesBeforeTheBodyIsRead` pins the ORDERING rather than the outcome: the
fixture flushes a 401 status and stalls before the body, and asserts the cache is already empty
at that instant. Moving the guard back below `ReadFrom` compiles and kills it — and also kills
the unreadable-body test, because the late position stops covering that arm. Two bugs, one
placement.

## Mutation

Two independent properties, two independent mutations, both compiling:

- **Neuter one provider's invalidator** (`_ = presented`) → kills ONLY that provider's 401
  tests; the other two stay green. Run for each of the three, the diagonal is clean. A single
  shared test would not have discriminated.
- **Remove the CAS guard** (revert to unconditional) → kills the ABA test in all three while
  every 401 test stays green, confirming the invalidation and the race are covered separately.

No survivors.
