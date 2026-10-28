# 2026-10-27 — a mutate that failed after a retry is no longer reported as definite

**Fix** — `doRequest` returns only `(body, err)`, so an idempotent `:mutate` call site could
not tell a first-attempt failure from one that followed a 429 retry. A 429 is Google
acknowledging receipt and a `:mutate` carries no idempotency key, so the rate-limited attempt
may still have been applied; a later definite refusal — including the `400` a second attempt
can earn *precisely because* the first one already took effect — proves only that the last
attempt changed nothing. Every such failure was reported as "the platform was not modified",
which on the budget and bid levers is false about money. Refs #347.

**The shape.** `doRequestCounted` and `doRequestValidatedCounted` are the existing methods
additionally returning the loop's `attempt` index at the point of return — exactly the number
of attempts that preceded the one being reported on, so `0` means the outcome answers the one
and only request sent. `doRequest` and `doRequestValidated` became thin delegations, so no
existing caller changed. `retriedUnconfirmed(what, retries, err)` is the one place the rule is
applied, so the five call sites each spend a single line and none of them can get the condition
subtly different. It wraps in `retriedUnconfirmedError` only when `err != nil`, `retries > 0`
and `!IsOutcomeUnconfirmed(err)`.

**What each clause protects.** A success is confirmed however many 429s preceded it — the
attempt that succeeded is the one being reported on. An UNRETRIED failure is the answer to the
only request sent, so a flat validation `400` stays definite; without that clause the fix would
make every failure ambiguous, which is its own defect, since an operator told to "verify
upstream" after a flat refusal learns nothing. And an error already ambiguous — by shape, or
through one of this package's explicit `Unconfirmed()` markers — passes through unwrapped,
which is also what keeps #384's budget-alignment marker in the shape
`CreateOutcome`'s `carriesUnconfirmedWrite` arm reads.

**The clause that reads wrong and is right.** An `errRequestNotTransmitted` failure on a LATER
attempt — a token refresh or request build that fails on attempt 2 — reports `NotAttempted()`,
so `IsOutcomeUnconfirmed` returns false and it IS wrapped. "Not attempted" is true of the
attempt that failed; attempt 0 was transmitted and answered 429. That marker was already gated
on `attempt == 0`, and its own comment already named #347 as the reason.

**The issue's table is wrong in two directions.** It lists five idempotent mutating
`doRequest(…, true)` call sites; a grep returns **six** — it omits `pmax_creative.go`'s
`assetGroups:mutate`. And of those six, only five need the change: `adGroupAds:mutate` is left
on `doRequest` because every failure there is already wrapped in `partialCascadeError`, which
reports `Unconfirmed()`, so `retriedUnconfirmed` would be a guaranteed no-op. The ad group
stage immediately above it does need the wrap, because nothing precedes it in that function and
its failure arm returns a plain error.

**The test drives the HTTP handler.** `retried_unconfirmed_test.go` answers the first request
with a retryable 429 and the second with a definite `400`, through the real retry loop
(`withRetryBaseDelay(time.Millisecond)`, so it does not sleep). Each of the three wrapped
levers is run twice — once with the 429 in front, once with the `400` alone — so the
counter-case is pinned alongside the fix. Against a neutered gate all three unconfirmed
subtests fail with the issue's exact symptom and all three definite subtests stay green.

**Sibling parity.** `reddit`, `twitter` and `microsoft` already carry a `retriedUnconfirmedError`
with the same gate; googleads was the outlier. `meta` and `linkedin` both retry idempotent
mutates and have **no** such marker — the same class of defect, not in this change's scope.
