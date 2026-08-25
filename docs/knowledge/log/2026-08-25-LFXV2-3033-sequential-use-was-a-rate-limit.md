# 2026-08-25 — "safe for sequential use" was a rate limit, not a race

**Fix** — X/Twitter was the remaining leg of LFXV2-3033's client-cache rollout (LinkedIn and Meta
are wired separately under cs#186, and the roster still lists them as unwired at the time of this
change). The comment in `credcache.go` explaining X's exclusion said this:

> X in particular documents its client as safe for SEQUENTIAL use only, so it must not be shared
> across concurrent callers on the strength of this pattern alone.

That reads as memory unsafety, and it is why the wiring sat deferred for weeks: sharing a client
that isn't goroutine-safe is a data race, and no cache is worth one. The claim was false.

`twitter.Client` had no mutable receiver state at all. Every field — `creds`, `account`,
`baseURL`, `apiVersion`, `httpClient`, `nonceFn`, `timeFn`, `writeDelay` — is written once in
`NewClient`/`Option` and only read afterwards. There was no mutex in the file because the struct
never needed one. What the package doc actually said was that the client "honors" X's **1
write-request-per-second limit** — a RATE constraint on the account, not a claim about concurrent
memory access.

## The hazard ran the opposite way

Reading it as unsafety inverted the risk. NOT sharing was the dangerous option.

`pace` was a blind unconditional sleep before each write:

```go
func (c *Client) pace(ctx context.Context) error {
	if c.writeDelay <= 0 {
		return nil
	}
	return sleepCtx(ctx, c.writeDelay)
}
```

That bounds the write rate only while one goroutine owns the client for a whole flow. Two
concurrent dispatches holding separate clients each slept their own second in parallel and then
POSTed at the same instant — ~2 writes/sec against a documented 1/sec budget, on a path that
spends money. The limit is per ACCOUNT, so it can only be enforced by the object every caller for
that account shares. Sharing is the precondition for correctness, not the risk.

## What the fix changes

`pace` now reserves the next write slot under the client's own `writeMu`, tracking `nextWrite` on
the instance. The lock is deliberately held ACROSS the wait: releasing it would let every queued
caller read the same deadline, wake together, and fire simultaneously — the exact failure being
fixed. Reads never call `pace` and stay fully concurrent, because X does not rate-limit them.

A cancelled caller does not advance the reservation. Advancing on the cancel arm would let an
aborted dispatch push the next real writer back by a slot no write ever used.

## Why a sequential test could never have caught this

The pre-fix `pace` passes any sequential assertion — including the one already in the suite,
`TestWithWriteDelayZeroDisablesPacing`. Only overlapping callers distinguish a per-call-site sleep
from a rate bound. The regression test runs 8 goroutines into one shared client and asserts the
spacing between adjacent cleared writes; reverting `pace` to the blind sleep produces gaps of
**1–42µs against a 5ms floor**, with the whole burst spanning 66µs instead of 35ms.

## The rule

A doc comment naming a constraint is a claim about a mechanism, and the mechanism decides the
fix. "Sequential use only" and "1 write/sec" both discourage sharing, but one means *you will
corrupt memory* and the other means *you will exceed a budget you can only enforce by sharing* —
opposite remedies. Re-derive the constraint from the source before inheriting a deferral built on
it, and when a comment you edit states the reason, make the correction explicit: the next reader
inherits whichever version survives.
