# 2026-10-06 — Slot-lock tests prove the wait with pg_blocking_pids

**Verification** — review of #260. `TestLiveClaimAndAdoptWaitForTheSlotLock` and
`TestLiveUpsertWaitsForTheSlotLock` inferred "blocked" from a 500 ms timeout, which passes when a
goroutine is merely slow to reach the database. They now poll `pg_blocking_pids` until PostgreSQL
shows the expected number of backends waiting on the lock holder's own backend (two for claim and
adopt, one for the upsert), the pattern `audience_lease_live_test.go` already uses. Removing the
lock from any of the three writers (claim, upsert, adopt) fails a test naming that writer. See
[internal/infrastructure/postgres](../code/internal-infrastructure-postgres.md).
