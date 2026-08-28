# 2026-08-28 — LFXV2-2641 throttle ambiguity on meta and reddit

**Fix** — A 429 PROVES the platform received the request, so any failure that
follows one leaves the mutation's outcome AMBIGUOUS rather than "not applied".
Meta and Reddit reported several such failures as definite, and the cost is
concrete: a caller told a create definitely failed retries it, and the retry
makes a second campaign that spends real budget.

Three paths were wrong, all of them downstream of a 429.

The BACKOFF SLEEP returned the bare `ctx` error on both clients. A bare
`context.DeadlineExceeded` matches none of the arms `createOutcomeAmbiguous`
keys on, so it fell through to false. The Google Ads client documents this exact
hazard and wraps its `ctx` error for precisely this reason; Microsoft and Twitter
do the same. Meta and Reddit were the two that did not.

Reddit's OVER-CAP ABORT returned a plain `fmt.Errorf`, which matches nothing the
classifier keys on. That path is reachable rather than defensive — it fires
whenever Reddit declares a rate-limit reset longer than `maxRetryWait`.

Reddit's classifier had NO 429 BRANCH at all, so an EXHAUSTED retry classified
as definitely-not-applied even though the 429 proves receipt. Meta already had
that branch with the reasoning written out; Reddit was the outlier.

**Verification** — The first version of the regression tests did not bind, and
that is worth recording. They raced a 20 ms context deadline against a real
backoff sleep, so the cancellation could land inside `httpClient.Do` instead —
a path that already wrapped its error before this fix. Reverting the fix left
them green.

Cancelling from inside the HTTP handler does not fix it either: the client is
still mid-exchange there, so the cancel kills the NEXT request and the error
reads `Post "...": context canceled`.

What works is releasing a goroutine once the handler has written the 429, then
cancelling after a short pause — the client is in `sleepCtx` by then. The tests
also assert the error is not a `Post "..."` failure, so a future change that
reintroduces the race fails loudly rather than passing vacuously. Reverting each
guard now fails its own test.
