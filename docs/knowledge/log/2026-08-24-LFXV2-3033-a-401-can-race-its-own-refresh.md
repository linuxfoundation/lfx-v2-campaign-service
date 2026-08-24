# 2026-08-24 — LFXV2-3033: a 401 can race the refresh that produced its token

**Fix** — The compare-and-clear guard on the reddit, microsoft and google-ads
clients cleared a rejected token only when it was still the cached one. That
condition was necessary (an unconditional clear evicted a *newer* token no 401
had rejected — the ABA bug) but not sufficient, because it assumed the cache was
the only place the rejected value could live.

The publication order says otherwise. `fetchToken` stores the token and
**unlocks**; only under a **later, separate** lock acquisition does the leader
goroutine set `inflight.token`, retract `c.inflight` and close `done`. Between
those two acquisitions the cache already holds the new token while the flight is
still joinable:

```text
leader: cachedToken = tok_1, unlock
A:      fast path takes tok_1 -> 401 -> invalidateAccessToken clears the CACHE
B:      cache empty, c.inflight still non-nil -> joins the flight
leader: inflight.token = tok_1, close(done)
B:      receives tok_1 — the token the platform just rejected
```

So "it is still cached, therefore it is the rejected one" does not hold, and an
in-flight refresh can be the *source* of the token a 401 rejected.

The fix keeps the two opposed failure modes apart by matching on **token
identity** in both places rather than widening the clear:

- a flight whose `token` equals the rejected value is blanked **and
  unpublished** from `c.inflight`;
- a flight carrying any other token is untouched, so a late 401 still cannot
  evict a genuinely newer token.

Blanking alone was not enough and the first attempt **stack-overflowed**: a
waiter that re-led found the same poisoned flight still published, rejoined it,
read the blank again and recursed without bound. Unpublishing is what makes the
retry start a genuinely new exchange.

Unpublishing in turn forced the leader's teardown to become a compare-and-clear
of its own (`if c.inflight == inflight`): once invalidation can retract a flight
early, a stale leader's unconditional `c.inflight = nil` would erase a *newer*
flight and strand every caller waiting on it.

**Verification.** Each arm was mutation-tested per provider; neutering one
provider failed only that provider's test. One survivor was found and killed:
`TestInvalidate_DoesNotStrandANewerFlight` originally *replayed* the teardown
branch inline, so it agreed with itself and survived the branch being deleted —
it now drives the real leader goroutine.

The body-stall ordering tests were also resynchronised. They sampled the cache
after a fixed `150ms` sleep, which is not synchronised with either the token mint
or the handler's flush: on a slow runner the timer can fire before a token was
ever cached, so the assertion passes against an empty cache that was never
populated — a false pass. They now rendezvous on a channel closed from inside the
handler and poll the cache, with a positive control asserting the token was
actually minted **and presented**. Google Ads had no ordering test at all (only
outcome-only, eventual-invalidation ones); it has one now, and it independently
catches a guard relocated after the body read.
