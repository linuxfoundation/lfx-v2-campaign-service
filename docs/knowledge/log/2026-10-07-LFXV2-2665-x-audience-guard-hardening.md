# 2026-10-07 — LFXV2-2665 X audience read: guard hardening (PR #291)

**Fix** — Five PR #291 review threads on the X audience read's load guard:

- **Joined result across midnight.** A "today" read joining a read started before the account's
  midnight took yesterday's buckets. A joined success is now returned only if its window still
  resolves to the joiner's account-local instants (`windowCurrent`, the cache's check); otherwise
  the joiner re-leads (same bound as a cancelled leader).
- **Abandoned jobs.** A failed read's jobs keep running on X, and failures are not cached, so
  refreshes could pile jobs toward X's 100-per-account limit and starve the account monitor. The
  client reports them (`twitter.AudienceJobsAbandonedError`); the guard counts them per account
  for 60 minutes (the monitor's existing `accountReportAbandonAfter`; X documents no job lifetime)
  or until `RunningStatsJobs` says they finished, and refuses (503) a read whose jobs
  (`AudienceJobCount`) would take the account past 12 outstanding — headroom left for the monitor.
- **Slots.** `g.slots` never shrank; an account's slot is now deleted when nobody holds or
  awaits it.
- **Atomic pacer reservation.** The backlog check was a snapshot: other writers could slip in
  between it and the paced POSTs and push a batch past its deadline. `reserveStatsJobSlots`
  reserves the whole batch's consecutive slots under `writeMu`, refusing before reserving when
  they do not fit; each POST waits for its own slot. The account monitor's submission uses the
  same reservation (replacing `statsJobsFitBudget`).
- **Flag docs.** `TWITTER_METRICS_ENABLED` gates the audience read as well as the monitor; the
  README, chart values, `pkg/constants`, the deployment and account-monitor concepts and the
  dispatcher comments now say so.
- Tests for each; each fails with its fix reverted.
