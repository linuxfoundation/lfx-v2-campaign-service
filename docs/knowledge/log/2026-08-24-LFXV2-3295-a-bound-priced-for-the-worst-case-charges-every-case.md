# 2026-08-24 — a bound priced for the worst case charges every case

**Fix** — the upload admission budget and the per-upload weight were both 128 MiB, so the
weighted semaphore admitted exactly ONE upload at a time, whatever its size.

The two constants were derived independently and each was defensible on its own. The budget is
`PodMemoryLimitBytes / 4`, because uploads may claim only a minority of a 512Mi pod. The weight is
128 MiB, because the ~30 MiB decoded slice, the up-to-80 MiB pixel buffer and the unreclaimed
body buffer provably coexist for one maximum-size upload. Neither derivation mentions the other,
and nothing in either one is wrong.

Their QUOTIENT is what shipped, and nobody derived it: `128 MiB / 128 MiB = 1`. A 200 KiB logo
was charged the same permit as a 30 MiB image, and while any upload was in flight every other one
was shed with 503.

## The consequence was availability, not memory

Because the permit is held across the whole of `next.ServeHTTP` — deliberately, so the socket
read cannot happen unadmitted — the hold lasts as long as the request does, including the body
read. With exactly one permit in existence, a single client dribbling a body held the ENTIRE
upload capacity of the service for as long as `DefaultReadTimeout` allowed (90s), and every
concurrent upload was refused. `replicaCount` is 1, so there was no horizontal mitigation. A
control added to prevent a memory-exhaustion denial of service had introduced a cheaper one.

## The fix prices the request rather than the route

`UploadAdmissionWeightFor` charges from the request's declared `Content-Length` at a fixed
amplification ratio, floored so tiny uploads are never free and ceilinged at the same worst-case
128 MiB. The AGGREGATE bound is unchanged — the semaphore still cannot issue more than
`UploadAdmissionBudgetBytes` — but the budget is now divisible.

Trusting a caller-supplied length looks like the weak point and is not, because the two
directions of the lie are asymmetric. Over-declaring charges the liar more and exhausts their own
admission first. Under-declaring is closed downstream: `MaxBodyBytes` wraps the body in
`http.MaxBytesReader`, so a request that understates its length still cannot read past
`MaxRequestBodyBytes` — it is refused with 413 at the read. A liar buys a cheap permit and cannot
spend more than the ceiling it would otherwise have been charged. Absent or chunked
(`ContentLength < 0`) is priced at the full ceiling, or "declare nothing" becomes the cheapest
permit on the route.

The acquire stays exactly where it was. Moving it later — past the body read, to cover only the
decode — would have narrowed the hold too, but it re-opens the hole this middleware exists to
close: `decodeRequest(r)` runs before `endpoint(...)` and `authJWTFn` is the endpoint's first
statement, so a permit taken after the read is a permit taken after an unauthenticated caller has
already allocated ~72 MiB. Pricing solves the same problem without touching the placement.

## Why every existing test agreed with the defect

There were five admission tests and they were good tests. Each supplied its OWN budget and weight
— `budget = 100, weight = 40 -> 2 concurrent` — precisely so it could not silently agree with a
mutated constant. That independence is correct for testing the semaphore's arithmetic, and it is
exactly what hid this: **no test ever instantiated the middleware with the constants the service
actually ships.** The mechanism was proven; the configuration was not.

The regression guard now wires the real `UploadAdmissionBudgetBytes` and the real pricing
function, and proves concurrency by RENDEZVOUS rather than by timing: N admitted handlers each
decrement a `sync.WaitGroup` and then block on it, so none can leave until all N are inside
together. If admission only ever lets one through, the barrier is never satisfied and the test
fails by timeout. There is no sleep and no elapsed-time threshold in the assertion — a
concurrency proof built on a duration passes on a fast machine for the wrong reason.

## The rule

When two constants are derived separately and then combined by the code, **the combination is a
third fact and needs its own derivation and its own test.** Ask what the quotient, sum or product
MEANS, and assert that meaning against the shipped values — not against test-local stand-ins,
which prove the mechanism while leaving the configuration unexamined.
