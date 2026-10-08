# 2026-10-07 — LFXV2-2665 X stats jobs: one pod owns them

**Fix** — Five #296 review threads:

- **Runtime single-owner lease.** The chart's replica guard cannot see an out-of-band scale or an
  external HPA (ArgoCD ignores `/spec/replicas`), so several pods could each run their own
  process-local X job budget and pacer. New `domain.StatsJobLease`, implemented by
  `postgres.StatsJobLease`: a session advisory lock per X ad account (two-int key space, no
  migration) on a dedicated connection. The X dispatcher asks it before the audience read
  contacts X and before the account monitor submits a report; a pod without the lease answers
  the audience read 503 ("another instance owns X stats jobs; retry") and the monitor's
  submission fails in its existing transient class (logged, saved report served). A held lease
  is re-verified (connection ping) on every call, so losing the session stops new submissions
  at the next call. The container binds it on both wiring paths and releases it before closing
  the pool. The chart guard stays as a first line of defence.
- **Effective flag value in the chart guard.** The guard checked `app.environment` and
  `app.extraEnv` independently, so a later extraEnv `"false"` still read as on. It now computes
  the value the container gets: the env list renders `app.environment` then `app.extraEnv`, and
  Kubernetes keeps a repeated name's last occurrence; a `valueFrom` on the effective entry counts
  as on.
- **JSON specs.** Verified on origin/main: all eight published documents (v2/v3, JSON/YAML,
  gen/ and kodata) already carry the same operation text — the JSON files are single-line, which
  hides them in a diff review. `make apigen` changes nothing. New
  `TestPublishedSpecsAgreeOnOperationText` turns any future drift into a failure.
- **Pacer comments.** `pace`, `TwitterDispatcher.clients` and the credcache roster no longer
  describe the write pacer as per client instance; it is per ad account (`AccountPacers`).
- **#298 review: a cancelled request keeps the lease; a distinct "unavailable" answer.** The
  liveness ping ran on the caller's context, so a cancelled or expired request made a healthy
  session look dead and destroyed the lock, letting another pod take the account while this one
  still had jobs outstanding. The ping now runs on a detached context with its own 2s timeout,
  and a failed ping on a request that has already ended KEEPS the lease (that request is refused);
  a dead context never starts an acquire. Failures to establish ownership (no database, a failed
  acquire or lock query, an ended request) are now `domain.ErrStatsJobLeaseUnavailable` ("coordination
  of X stats jobs is unavailable; retry"), mapped to the same 503 with its own fixed text
  and logged at warn; only a lock Postgres reports as held elsewhere is
  `ErrStatsJobLeaseNotHeld`.
- **#298 review: leases on one session outside the pool; v2-vs-v3 spec check.** Each owned
  account pinned a connection from the SHARED business pool for the life of the process, so a
  small pool (`pool_max_conns=1` is a real configuration) or a few X accounts could starve
  ordinary requests and readiness. All of a pod's leases now sit on ONE dedicated session opened
  with `pgx.ConnectConfig` from the pool's config — one connection beyond the pool — serialised by
  a mutex; a lost session drops every lease, and each account re-acquires on its own next check.
  A connect failure is `ErrStatsJobLeaseUnavailable`. The spec-agreement test reset its baseline
  per v2/v3 pair, so a v2-only or v3-only drift passed; it now compares all eight documents
  against one baseline by operationId, and a test perturbs a v3-only copy to prove it.
- Opening the lease session ran under the lease mutex, bounded only by the caller's context and
  the DSN's `connect_timeout`, so a black-holed database could stall every admission check and
  `Close`. It is now bounded by `statsJobLeaseConnectTimeout` (5s);
  `TestStatsJobLease_SessionConnectIsBounded` points the lease at a listener that never answers.
- **#298 review: one account's failure keeps the other leases; a bounded shutdown close.** A
  failed lock query for an account the pod did not yet hold dropped the shared session and with
  it every other account's lease, letting another pod take unrelated accounts with jobs in
  flight. Now the session is dropped only if a ping shows it dead; otherwise that key is
  `pg_advisory_unlock`ed (the failure may have come after the lock was granted) and only that
  account answers Unavailable. The lease close at shutdown was an unbudgeted phase that replaced
  the caller's deadline with a fresh 5s: it now has its own reserved slice of
  `ContainerCloseTimeout` (`statsLeaseCloseTimeout`, 250ms) and `Close` honours the caller's
  deadline.
- Tests for each; each fails with its fix reverted.
