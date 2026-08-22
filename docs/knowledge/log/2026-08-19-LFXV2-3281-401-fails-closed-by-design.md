# 2026-08-19 — LFXV2-3281 the 401 arms fail closed, deliberately

**Note** — a review round asked for refresh-and-retry on a 401 when `CanRefresh()` is true,
on both the request path (`client.go`) and the metrics path (`metrics.go`). Recording why the
answer is no, since the absence of a retry looks like an oversight and will be re-raised
otherwise.

## The constraint is the one already written into doRequest

`doRequest`'s 429 handling draws the line:

> Plain create POSTs (campaign-group, campaign, post, creative — no PARTIAL_UPDATE header)
> stay NON-idempotent: those endpoints carry no idempotency key, so a 429 whose first attempt
> may already have committed upstream must NOT be re-sent (it would create a DUPLICATE
> resource).

A 401 establishes nothing more than a 429 does about whether the write landed. LinkedIn can
reject a token *after* accepting and acting on a request, and `CreateCampaign` is a cascade of
four sequential POSTs, so a replay at the wrong point leaves a duplicate campaign group or a
second dark post that nothing reconciles.

Adding refresh+retry to the shared `doRequest` would therefore apply it to exactly the
requests that rule forbids replaying. Gating it on the existing `idempotent` predicate would
be safe, but it would then skip the create cascade — the path this ticket was opened for — so
it buys the least where it is wanted most.

## What fail-closed actually costs

Less than it appears. Dispatch constructs a fresh `Client` per operation, so the next
operation starts with an empty cache and refreshes cleanly. The operator sees one failed
operation, not a stuck connection.

The misleading part was the comment, not the behaviour: `invalidateAccessToken` said "the next
caller re-exchanges" without saying that the next caller is a SUBSEQUENT OPERATION, never the
one that just failed. Both 401 arms return immediately after calling it. The godoc now says so
and records the idempotency reasoning inline, so the next reader does not have to rediscover
it.

## The contract now has a regression seam

`TestRefreshCapable401FailsClosedWithoutReplay` covers the case none of the existing 401 tests
reached: they all build bearer-only credentials, so `CanRefresh()` is false and the decision
was untested in either direction.

The load-bearing assertion is the exchange COUNT, not the returned error. Asserting only
`ErrCredentialsExpired` would pass equally against an implementation that silently retried and
then gave up, which is the behaviour under discussion — so the test pins one API call and one
token exchange, plus the invalidated cache.

**Mutation:** adding `if c.creds.CanRefresh() && attempt == 0 { continue }` to the 401 arm
fails it on both counts (`api calls = 2, want 1`; `token exchanges = 2, want 1`).

If the fail-closed choice is ever revisited, the safe shape is to gate the replay on the same
`idempotent` predicate the 429 path uses, and to keep the two 401 arms behaviourally
identical — a contract where metrics retries and creates do not is the divergence that makes
the next reader wonder which one is the bug.
