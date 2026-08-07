# 2026-08-07 — LFXV2-2665: an unread Graph envelope is not evidence that Meta rejected the create

**Update** — `createOutcomeAmbiguous` recognises a throttled create by the Graph rate-limit `Code`
on the `*APIError`, because Meta reports rate limiting as an HTTP 400 carrying that code far more
often than as a 429. Two branches in `doRequest` return a non-2xx `*APIError` with no code at all,
and the classifier read that absence as "Meta sent none" when it actually meant "we never got it".

**Fix** — The oversized-body branch returns before `env` is unmarshalled, and could not have
populated it in any case: `raw` holds only the first `maxResponseBody+1` bytes of a larger body, so
it is truncated JSON. The read-error branch below it does carry a parsed envelope — but only when
the partial body happens to parse, which is the common shape and not the only one. In both, a
throttled create arrived as a bare 400 with `Code == 0`, which reads as a clean semantic rejection:
`createOutcomeAmbiguous` returns false, the retained claim is released, and the retry duplicates a
PAID campaign against a create Meta may already have committed.

**Fix** — `APIError.EnvelopeUnreadable` records the distinction the existing fields cannot, and
`createOutcomeAmbiguous` treats it as ambiguous. It is set on the oversized branch unconditionally,
and on the read-error branch only in the `env.Error == nil` arm — where the truncated body did not
parse, so the paragraph that carries the code forward does not apply.

**Fix** — Three tests, and the third is the one that keeps this from being a widening.
`TestDoRequestOversized400StaysAmbiguous` and `TestDoRequestUnparseableBody400StaysAmbiguous` pin
the two branches at status **400**; the existing oversized test uses 500, which is ambiguous on
status alone and therefore could never have detected this. `TestReadableRejection400StaysClean`
pins the opposite direction: a 400 whose envelope parses and carries a non-throttle code is a
definite rejection and must stay a CLEAN failure, or every genuine rejection becomes unconfirmed
and an operator is sent to Ads Manager to look for a campaign that was never created. Deleting the
classifier arm fails the first two and leaves the third green.
