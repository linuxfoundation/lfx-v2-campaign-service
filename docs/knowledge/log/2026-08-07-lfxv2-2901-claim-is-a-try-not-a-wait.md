# 2026-08-07 — LFXV2-2901: the campaign claim is a TRY, not a wait

**Update** — Both campaign advisory-lock sites (`ClaimCampaignVersion` and
`DeleteCampaign`) now use `pg_try_advisory_lock` instead of the blocking
`pg_advisory_lock`. Copilot found the cost of waiting: `pg_advisory_lock` blocks only
AFTER the request has already checked a connection out of the pool. The winning claim is
held across the ad-platform call — up to 45 seconds — so every LOSER would hold a SECOND
pooled connection for that entire span. A small burst against one campaign could then
exhaust a finite pool and stall unrelated API work and the readiness probe. Failing fast
keeps contention costing one connection per campaign rather than one per request.

**Fix** — Added `domain.ErrCampaignWriteInProgress`, mapped in `mapBriefErr` to a 409
whose message tells the caller to retry shortly. It is deliberately NOT
`ErrPreconditionFailed`: the loser's `If-Match` may be perfectly current — it lost a
race, not a version check — so a 412 would send it off to refetch and rebuild a request
that was already correct, and a client that automates that never retries the write at
all. It is also not `ErrConflict`, whose "the resource already exists" wording describes
a uniqueness violation and so reads as permanent.

**Fix** — On a definite `false` the connection is RELEASED, not destroyed. The
destroy-the-connection rule applies where Postgres may have granted the lock while the
client saw a failure (a cancelled context mid-call), because a session advisory lock is
not released by returning the connection to the pool. A `false` is Postgres answering: no
lock was taken, nothing is stranded, and destroying a healthy connection would be pure
churn. The error path from the `QueryRow` itself still destroys via `closeLockConn`.

**Fix** — `internal/domain/brief_port.go` had said a second caller's claim "blocks". It
now states the try-lock contract and forbids implementations from making the loser wait,
so the reason for the choice travels with the port rather than only with the Postgres
adapter.

**Fix** — `TestBriefService_LostClaimIsARetryable409` covers both writers that claim
(update-campaign and delete-campaign, which map errors differently — delete intercepts
`ErrConflict` itself before delegating). It asserts against 412 and against the
"already exists" wording specifically, since those are the two wrong answers that no
other test would catch. Verified binding by removing the `mapBriefErr` case (both
subtests fail with 500) and by mapping the sentinel to 412 instead (both fail naming the
wrong advice).

**Fix** — `TestClose_CooldownStopSpendsExactlyItsReservedTerm` closes the other half of
the shutdown-budget invariant flagged in the same round. `TestShutdownBudgetComposes`
proves `cooldownStopTimeout` is RESERVED; this one proves `Container.Close` actually
hands that same constant to `StopCooldownsForShutdown` (the argument becomes each woken
release's own unlock deadline, so a different value silently restores the 5s
`lockReleaseTimeout` and pgxpool.Close blocks for the difference outside
`ContainerCloseTimeout`), and that the signal precedes `pool.Close()`. Verified binding
against both a substituted constant and a reordered call.
