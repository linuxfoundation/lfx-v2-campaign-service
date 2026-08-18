# 2026-08-18 — LFXV2-3222 terminal campaign_jobs retention

**Creation** — `campaign_jobs` was append-only and unbounded: nothing in the service ever
deleted a row, so the table grew with every brief dispatch forever, and `000004`'s stuck-job
recovery sweep paid for that growth on every pass across every replica.
`JobRepo.PruneTerminalJobs` plus `Orchestrator.StartJobRetentionSweeper` bound it, deliberately
shaped after the existing outbox prune (`PrunePublishedIndexMessages` / `Relay.prune`) rather
than as a new mechanism — a bounded batch per pass, run periodically, on every replica with no
leader election.

**These rows are the audit trail of real ad spend**, and that fact — not table size — drove
every decision below. A campaign job records that a brief was dispatched, to which platforms,
and what each one returned. Deleting one destroys the only in-service record of a paid campaign
creation, so every ambiguous case resolves toward keeping data.

**Terminal statuses are an ALLOW-LIST, never a negative predicate.** `terminalJobStatuses`
(`succeeded`, `partial`, `failed`) is passed as `status = ANY($1::text[])`. `status != 'running'`
or `NOT IN ('queued','running')` would pass every behavioural test written against today's five
statuses and then silently sweep in a status added later — deleting spend records with no code
change and no review. Derived from `model.JobStatus.Terminal()` in
`internal/domain/model/campaign.go` and the `campaign_jobs_status_check` CHECK constraint in
`000002`, and pinned in both directions over the WHOLE vocabulary by
`TestTerminalJobStatusesMatchTheDomainVocabulary`, so a future status must be classified
deliberately instead of defaulting into deletion.

**A queued/running row is never eligible, at any age.** An old non-terminal row is not stale
history — it is a STUCK JOB, which is exactly the record someone needs to investigate a
dispatch that never finished. Nothing is retained forever as a result: `FailStuckJobs`
transitions those to `failed` after `staleJobCutoff`, at which point they become terminal and
their retention window starts from that transition.

**Age is measured on `updated_at`, not `created_at`.** `updated_at` is when the job REACHED its
terminal state, which is the age retention is defined against. A job created months ago but
completed yesterday is recent history, and `created_at` would prune it first.

**A non-positive window selects the default, not "delete everything".** The sweeper passes `0`
whenever `CAMPAIGN_JOB_RETENTION` is unset, empty or unparseable — the common case — and
`updated_at < now() - '0s'` matches every terminal row. Both `PruneTerminalJobs` and
`SetJobRetention` treat non-positive as "unset". `parseRetention` likewise returns 0 for
`"30 days"` and `"7d"` (neither is a valid Go duration, and both are the plausible typos)
rather than coercing them. This does NOT panic the way `EVENT_URL_NAT64_PREFIXES` does: there a
bad value silently WIDENS an SSRF guard, so refusing to boot is right; here the fallback keeps
MORE history than asked for, and failing to start over a retention typo would turn a cosmetic
misconfiguration into an outage.

**`DefaultJobRetention` is 180 days**, and `TestDefaultJobRetentionIsSafe` pins the direction
(months, not the exact figure) so it cannot drift short unnoticed. The chart sets `4320h`
explicitly in `values.yaml` so the retention actually in force is visible there rather than
implied by the binary.

**Migration `000026`** adds partial index `idx_campaign_jobs_retention` on
`campaign_jobs (updated_at) WHERE status IN ('succeeded','partial','failed')`. `000004`'s
`idx_campaign_jobs_recovery` is partial over the two NON-terminal statuses — the exact
complement — so it cannot serve this predicate, and without a new index the prune full-scans
the very history it exists to bound. A new numbered pair, never an edit to `000004`:
golang-migrate records applied versions and never re-runs them.

**No `FOR UPDATE SKIP LOCKED`, unlike the outbox DRAIN.** The drain needs it because it claims
rows it will then publish — an at-most-once side effect outside the transaction, so two pods
claiming the same row double-publish. A DELETE has no such side effect: it takes its own row
locks, and overlapping replica sweeps simply find fewer rows each. The outbox PRUNE is the
right comparison and does not use it either. Multi-replica safety here needs only that the
statement be bounded and idempotent.

**Mutation-verified, against a live PostgreSQL 16.** Each guard was reverted with a COMPILING
change and the suite re-run: a negative predicate (live test observed a `queued` row deleted);
the `LIMIT` removed (one pass deleted 6 rows under a bound of 2); `created_at` swapped for
`updated_at`; the 180-day default shortened; `queued` added to the allow-list; the index
predicate flipped to the non-terminal statuses (caught by reading `pg_indexes` on the applied
schema); the config fallback returning a short window; and `SetJobRetention`'s non-positive
guard removed. All eight failed as intended.

**The zero-window fallback was initially UNTESTED** — reverting it changed no test, which is
the definition of a guard that does nothing. That gap mattered more than the others, because
the sweeper passes `0` on every deployment that never sets the variable, so the untested path
was the DEFAULT path: unguarded, the first sweep would have deleted the entire spend history.
`TestLivePruneTerminalJobsTreatsAZeroWindowAsTheDefault` now covers exactly the call the
sweeper makes.

**`TestEveryConfiguredEnvVarIsWiredInTheChart` caught the missing chart wiring** before review
did — a new `pkg/constants` env var that `values.yaml` never injects is a feature silently
disabled in every deployed environment.
