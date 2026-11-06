# 2026-11-06 — Reddit report budget: a shared Postgres ledger replaces the per-process one (#380)

**Update** — The Reddit reporting budget (`internal/platform/reddit/report_budget.go`: 45 of an
ad account's 60 report requests a minute, 24 of them for the audience and keyword reads) was kept
in each process's memory, so N replicas could spend N × 45 — 135 at production's three. Its slots
now live in Postgres: `postgres.RedditReportLedger` on the new `reddit_report_slots` table
(migration 000055), driven by `reddit.NewSharedReportBudgets`, which every
`dispatch.RedditDispatcher` gives its clients and the container binds on both wiring paths
(`bindRedditReportLedger`). The account-wide ceiling is 45 at any replica count.

- **Atomic:** a `Take` is one transaction — `pg_try_advisory_xact_lock` on the account, then one
  statement that counts the window and inserts only when both limits have room — on the
  database's clock (`clock_timestamp()` after the lock). A busy lock is answered at once with a
  short wait (never a blocked pooled connection) and re-asked after a jittered sleep.
- **Fails closed:** an unbound ledger, a Postgres error or the 5 s bound refuses the attempt with
  `reddit.ErrReportBudgetUnavailable` (unclassified → 503) before anything is sent.
- **Contract kept:** the slot is still taken immediately before the send; a pre-send dial failure
  deletes its row; no slot before the deadline still fails the read whole (503).
- **Not a refilling token bucket:** a bucket of 45 refilled at 45 a minute admits up to 90 in one
  60 s window, past Reddit's 60, so the ledger stays a sliding window of slot rows.
- **Pattern looked for first:** the X stats-job budget (#298) is a lease that makes non-owner pods
  refuse — rejected in the issue for the user-facing monitor. The ledger reuses the lease's and the
  keyword-targeting lock's two-int advisory-lock convention (class "RRPT") instead.
- `cmd/verify-metrics` (read-only database session) keeps a process-local ledger at the production
  sizing (`reddit.NewLocalReportBudgets`). It is unshared and NOT inside the 15-request headroom,
  so it is for off-peak use.
- Ledger errors are redacted against the pool's DSN (`newSafeDSNError`) before they reach a log.
- The Reddit account monitor classes an exhausted budget as `throttle`; a ledger failure is logged
  at warn where it is raised.

Tests: two ledgers on two separate pools race 120 Takes and are granted exactly 45 (and 24 insight
slots); two shared budgets on one ledger send exactly 45 between them; unbound and erroring ledgers
send nothing; a pre-send dial failure releases the shared slot. Every new test was mutation-checked
(advisory lock removed, insight cap removed, release no-op, wait dropped, fail-open on error or
unbound, binding ignored, dispatcher not passing the budget). The chart, `values.yaml`, the
deployment and Reddit architecture pages and `docs/api-catalog.md` lose the "per process" and
"N × 45" wording.
