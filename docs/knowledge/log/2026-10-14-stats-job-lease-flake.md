# 2026-10-14 — X stats-job lease: release-on-close test flake

**Fix** — `TestLiveStatsJobLease_OneOwnerPerAccountAndReleaseOnClose` failed intermittently
with "backend … still holds the lease after Close" (about 3% of runs under parallel load, rarer
in isolation). It read `pg_locks` from another connection immediately after `Close` returned.
`Close` closes the lease session through pgx, which sends `Terminate` and closes the socket
without waiting for the server; Postgres releases the session's advisory locks only as that
backend exits, asynchronously to the client. The read could therefore land before the backend
had exited.

A test bug, not a product bug: during that window the lock is still held, so another instance's
`pg_try_advisory_lock` answers "held elsewhere" (`ErrStatsJobLeaseNotHeld`), never a second
owner, and the closed lease refuses every `Own`. The single-owner guarantee is unchanged. The
test now polls `pg_locks` and `pg_stat_activity` (10ms ticks, 5s deadline) until no backend
holds the lock and the closed backend is gone, and only then asserts the handover. 0 failures in
200 sequential runs, 400 runs across 8 parallel processes, and 200 runs under `-race`. The
`StatsJobLease` section of `code/internal-infrastructure-postgres.md` now states that release
follows `Close` promptly but asynchronously.
