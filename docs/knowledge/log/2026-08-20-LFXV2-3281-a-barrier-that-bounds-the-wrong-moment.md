# 2026-08-20 — LFXV2-3281 a barrier that bounds the wrong moment

**Fix** — two defects from a second triage of SUPPRESSED Copilot comments on PR #148. The
first is a THIRD attempt at the same test, and the durable value is not the fix but why the
second attempt — which was reported as verified, with an argument for its own soundness —
was wrong.

## A barrier bounds where you place it, not where you need it

`TestAccessTokenValueSingleFlight` asserts that 25 concurrent callers produce exactly ONE
token exchange. Three attempts:

1. `time.Sleep(50ms)` in the handler, claiming the delay "guarantees the followers arrive
   while the leader's fetch is in flight". It guarantees nothing.
2. A test-side arrival barrier: each goroutine sends on `arrived`, the test waits for all N,
   then closes `release` to let the handler return.
3. The barrier signalled from INSIDE `accessTokenValue`, under `tokenMu` and past the cache
   read.

Attempt 2 is the one worth recording, because it was defended by an explicit argument and the
argument was wrong in a way that reads as sound:

> A signal is sent immediately before the call, so a goroutine could be descheduled between
> the two — the barrier bounds when callers START, not when they reach the cache check. That
> residual gap is closed by the DIRECTION of the remaining race: a straggler that arrives LATE
> finds the cache still EMPTY, because `release` is closed only after this loop and no exchange
> can complete before then. Late arrival therefore produces an EXTRA exchange, never a
> spuriously coalesced one.

The premise "a late arrival finds an empty cache" holds only WHILE `release` is unclosed. But
closing `release` is the next statement: the leader completes, writes `c.accessToken`, and any
goroutine still descheduled between its signal and its cache read now finds a WARM cache,
takes the fast path, and returns without ever consulting `inflight`. The direction of the race
inverts at exactly the moment the barrier stops holding it. **A gap that is safe during the
barrier is not safe after it — and the test's own next line ends the barrier.**

## The mutation that distinguishes them

Deleting the `inflight` join alone does not discriminate: on an idle machine all 25 callers
enter before the leader finishes, so even attempt 2 reports 25 and fails. The mutation has to
reproduce the hole — remove the join AND stagger callers past the leader's completion:

| test | mutation | result |
| --- | --- | --- |
| attempt 2 | join removed, 10 of 25 staggered | `exchanges = 15` — the 10 stragglers hit the warm cache |
| attempt 2 | join removed, 24 of 25 staggered | `exchanges = 1` — **PASSES**; a full survivor |
| attempt 3 | join removed, 24 of 25 staggered | `exchanges = 25` — **FAILS** |

The 15 is the mechanism made visible: exactly the staggered callers went missing from the
count. Under enough stagger the surviving count reaches 1 and the test passes its own negation,
which is what attempt 1 did too. **Two different barriers, the same defect, because both bound
a moment on the caller's side of the cache check.**

## Why a hook in production code earns its place

`Client.pastCacheCheck` is nil in production and called once, under `tokenMu`, immediately
after the fast-path read. It is a test seam in shipped code, which needs justifying rather than
assuming: the package already carries `withTokenURL` and `withRetryBaseDelay` as unexported
test-only Options, so the pattern and its visibility limits are established.

The justification is that the property is otherwise untestable. Every synchronization point
available to the test is on the caller's side of the cache read; the ordering the test needs —
"the leader's response is released only after the last caller has consulted the cache" — names
a moment that exists only INSIDE the function. No amount of test-side cleverness reaches it,
and the alternative on offer is a test whose correctness depends on the scheduler, which is not
a test. The cost is one nil check on the token path.

## An arm nobody would notice going missing

`brief.go`'s adoption path received the `ErrServiceDefect` mapping in the same round as five
other consumers, and `service_defect_status_test.go` covered five of the six — metrics, toggle,
discovery, the brief-metrics row, and the async dispatch log. Not adoption. Deleting the
adoption arm restored the misleading 409 with the ENTIRE `internal/service` package still
green.

Adoption's 409 is the worst of the six to get wrong: its switch carries three other
`ConflictError` arms, each naming a different remedy ("repair the connection", "select an
account", "connect your own ad account"), and a caller cannot tell them apart. Now covered by
`TestServiceDefect_AdoptionAnswers500NotAConnectionRepair409`, injecting through the lookup —
the path where the dispatcher resolves credentials, so the production shape.

All six sites were then swept rather than the named one: each arm deleted in turn against a
compiling build, each killing exactly one test, no survivors.

**A mapping added at six sites in one change needs coverage derived from the six, not from the
ones the test file happened to reach.** The five that were covered made the file look
comprehensive, which is why the gap survived a round of review.
