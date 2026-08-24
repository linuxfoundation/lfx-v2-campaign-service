# 2026-08-24 — a shared backoff schedule, not two that happen to agree

**Fix** — the `/adimages` throttle retry added on this PR advertised "the same capped
exponential backoff `do()` uses" and did not implement it. `throttleWait` had no `attempt`
parameter at all, so its no-`Retry-After` fallback returned `c.retryBaseDelay` unconditionally.
With the production 1s base the upload retried at **1s/1s/1s instead of 1s/2s/4s**, exhausting
its budget in roughly 3s where `do()` would have spent 7s. A throttle clearing in between is
exactly the case the retry was added for, and it left a terminal `created_degraded` campaign —
the campaign and ad set already exist by the time the upload runs, and no re-dispatch repairs
that status.

The comment at the site had accurately described the defect ("the attempt number is not threaded
in, so this uses the base delay") while the comment above it claimed parity with `do()`. A
truthful note about a shortfall is not the same as the shortfall being intended.

**The fix is structural rather than local.** Copying `do()`'s shift into `throttleWait` would
have produced two independent expressions of one policy — the arrangement that drifted in the
first place. Instead both paths now call one `(*Client).backoffDelay(attempt)` helper: `do()`'s
retry loop and the upload loop resolve to the SAME schedule by construction, so a future edit
cannot make them disagree without deleting the helper. `attempt` is threaded from
`uploadImage`'s loop through `uploadImageAttempt` into `throttleWait`, because the shift is
computed on it.

**Making the delays observable without sleeping.** Asserting a backoff by elapsed wall time is
both slow at the production base delay and unable to discriminate: a wait that was never
requested and a goroutine that was merely not scheduled look identical to a timing threshold.
Two mechanisms avoid it. `throttleWait` RETURNS its duration rather than sleeping, so the
schedule reads directly as a value per attempt. And a new unexported `withSleepFn` option
replaces the inter-retry wait with a recorder, so the sequence `uploadImage` actually REQUESTS
is captured in microseconds of runtime at the real 1s/2s/4s values.

**A mutation that survived, and what it exposed.** Pinning `throttleWait`'s attempt to the base
delay failed the schedule tests, and dropping the cap failed the cap test. But replacing the
loop's `attempt` argument with a constant `0` left every one of them GREEN: those tests call
`throttleWait` directly and so pin the schedule without binding the CALL SITE that supplies the
attempt. The survivor is the finding. `TestUploadImageLoopThreadsItsAttemptCounter` was added to
observe the delays end-to-end through `uploadImage`, and it kills that mutation. Note also that
attempt 0 agrees under both the correct and the broken schedule — a test checking a single
attempt would have passed against the bug, which is why the full sequence is asserted.

**Docs.** `docs/knowledge/code/internal-platform-meta.md` still stated flatly that creates go
through `doCreate` and suppress the throttle retry. That general rule is CORRECT and is left
standing: 429 retry eligibility is an explicit idempotency decision, and Meta exposes no create
idempotency key, so a re-POST of a shed create can duplicate a paid object. What was missing is
that `/adimages` is the one create-shaped call that passes the idempotency test — Meta
content-addresses ad images, so posting identical bytes twice returns the same hash and creates
nothing — and therefore legitimately retries. The entry now states the exception together with
the property that justifies it, rather than weakening the rule. A sweep of the other durable
client concepts found no other site asserting the blanket claim: googleads, microsoft, hubspot,
linkedin and twitter already frame retry eligibility as an idempotency test, and
`internal-platform-llm.md` already names its own departure explicitly.

**A second divergence from the same class, found by sweeping rather than by the thread.** A
suppressed finding on the adjacent line reported that `uploadImageAttempt`'s throttle arms were
gated on `readErr == nil`. That is REAL and is a distinct defect from the backoff shape: a 429
whose body is truncated — a mismatched `Content-Length` makes `io.ReadAll` return "unexpected
EOF" — fell through to the default arm and returned as FINAL, so a retryable rate limit became a
terminal `created_degraded` campaign on the strength of a body the STATUS had already made
unnecessary. `do()` does the opposite deliberately: it computes `isThrottle` from the status
BEFORE consuming `readErr`, and does not short-circuit a throttle it is about to retry. The
status-only arm is now ungated, with the Graph-envelope arm still ordered first so a READABLE
429 keeps its `Type`/`Code`/`FBTraceID` and only an unparseable one is marked
`EnvelopeUnreadable`. Both defects are the same class — the upload path re-deriving a policy
`do()` already owns — which is the argument for the shared helper rather than a second copy.
