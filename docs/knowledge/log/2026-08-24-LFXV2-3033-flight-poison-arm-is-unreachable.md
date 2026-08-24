# 2026-08-24 — LFXV2-3033: the flight-poison arm is unreachable, and said so wrongly

**Docs** — Doc-only correction to the token-invalidation guard on the reddit,
microsoft and google-ads clients. No behaviour changed; the arm and its tests
are untouched.

The guard's comments claimed the flight-poison arm
(`c.inflight != nil && c.inflight.token == presented`) covered a live race:
`fetchToken` caches the token and unlocks, and "in that window the cache already
holds the new token while the flight is still joinable". The companion test
`TestInvalidate_DoesNotLeakThroughAPublishedFlight` seeded
`c.inflight = &tokenRefresh{done: closed, token: "tok_1"}` and called that
"exactly the state fetchToken leaves behind between its cache write and the
leader's teardown".

**That state does not occur.** The leader sets `inflight.token`, runs its
compare-and-clear retract of `c.inflight` and closes `done` inside **one
unbroken critical section**. A published flight is therefore never observable
holding a non-empty token: by the time the token is set, the flight is already
unpublished under the same lock hold. The earlier reasoning read the two lock
acquisitions in `fetchToken` and the leader goroutine as leaving a gap between
publication and token-set, but the gap is before `inflight.token` is written,
not after — which is precisely open issue #180, not this arm.

**How it was established** (not by re-reading the code): an instrumented probe
sampled `c.inflight` under the real mutex while 16 goroutines drove
refresh/invalidate concurrently for 3s.

- at head: **~6.93M** observations of a published flight, **0** carrying a
  non-empty token;
- with the leader's critical section split by a single `Unlock(); Lock()`:
  the same probe saw the state **~73,823** times.

The second run is the control that makes the zero meaningful — the probe can
see the state when it exists, so the zero is a property of the lock discipline
rather than of the probe. A probe that never fires proves nothing on its own.

**Disposition — the arm is KEPT, only the comments changed.** Unreachability
here is a property of *today's* callers, not an invariant of the type, and the
distance to reachability is exactly the one-line `Unlock/Lock` split above —
the same shape an attempted #180 fix would introduce. The per-provider mutation
diagonal shows the arm is enforced, not dead weight: neutering it in one
provider fails that provider's test and only that provider's (3x3, no
survivors, every mutant compiled). Deleting a guard that is currently
unreachable but test-bound would remove the tripwire that would catch a future
#180 fix regressing it.

The lesson generalises the one recorded for cs#172: a correct behaviour
defended by an impossible claim is worse than an undocumented one, because the
next maintainer reads the claim as coverage and stops looking. Here the claim
would have hidden #180 twice over — once in the guard comment and once in a
test whose docstring said it reproduced the very window it does not reach.

The prior fragment `2026-08-24-LFXV2-3033-a-401-can-race-its-own-refresh.md`
carries the superseded rationale; it is left in place per the one-file-per-entry
rule, and this entry is the correction of record.
