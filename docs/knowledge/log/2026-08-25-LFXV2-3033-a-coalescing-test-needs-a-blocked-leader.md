# 2026-08-25 — a coalescing test needs a blocked leader, not just a burst

**Fix** — `TestClientCache_TwitterColdKeyConcurrentBuildsAreCoalesced` was meant to pin
`clientCache.buildOnce`'s singleflight: N concurrent cache misses must build ONE client, not N.
It passed against a `buildOnce` with `c.group.Do` deleted. Three separate defects had to be
removed before it bound, and each earlier version looked correct.

## 1. The wrong cache was under test

Resolving per goroutine coalesces at `decryptOnce` FIRST. The credential-cache leader finishes its
build and `put` before the followers reach `buildOnce`, which then serves them a WARM entry — so
the test verified the credential cache and never exercised the client cache's cold path. The Meta
version of this test carries the same warning; the fix is to resolve ONCE up front and share the
result, releasing every goroutine directly into `cachedTwitterClient`.

## 2. Instance identity only detects a LOST race

Comparing `client != got[0]` is the natural assertion and is what the Reddit, Microsoft and Meta
versions use. It detects a missing singleflight only when two callers actually reach `get()`
before the first `put()` lands. Those providers build a client that performs a token exchange
against an httptest server, so their window is wide. `twitter.NewClient` is a struct literal with
no I/O: the leader's `put` beats every follower, and all 16 callers receive the same instance with
or without coalescing. Measured against a singleflight-free `buildOnce`: **1 distinct client**,
every run.

Counting constructions — a `twitter.Option` runs exactly once per `NewClient` — observes the
coalescing directly instead of inferring it from a race outcome.

## 3. Counting alone was still only ~3/10

Even counting, the leader usually completed `build`+`put` before the next caller called `get()`,
so the uncoalesced case still often produced one construction. The fix is that the counting Option
runs INSIDE `twitter.NewClient`, therefore inside `build()`, therefore inside the leader's
critical section. Blocking there until every caller has arrived makes both outcomes deterministic:
with coalescing exactly one build starts and the other fifteen park in the flight (released by a
deadline); without it all sixteen enter `build` and the barrier fills at once. **10/10 against the
mutant, reporting 16 constructions instead of 1.**

## The rule

For a coalescing test, a burst of callers is the setup, not the assertion. What makes it bind is
that the LEADER IS STILL INSIDE the critical section when the followers arrive — otherwise the
fast path hides the very serialization being tested. If the real `build` is cheap, the test must
supply the blocking itself, at a hook the production code genuinely runs.

And the general form: when a test asserts a downstream EFFECT (which instance came back) rather
than the mechanism (how many builds ran), it can only fail when a race is lost. A cheap build
means that race is almost never lost, so the test is green for a reason that has nothing to do
with correctness. Prefer counting the mechanism.
