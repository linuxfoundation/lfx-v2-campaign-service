# 2026-10-07 — LFXV2-2665 X audience read: fail closed on job reconciliation

**Fix** — Three PR #291 review threads:

- **Reconciliation trust.** `twitter.Client.RunningStatsJobs`, which tells the audience guard
  which abandoned stats jobs X still runs, decoded the status answer last-wins: a repeated id
  (PROCESSING, then SUCCESS) or an unrequested one could release a still-running job from the
  account's outstanding-job budget. It now applies `readAudienceJobs`' rules — an answer naming a
  job not asked about, or one job twice, is an error, on which the guard keeps counting every job.
- **Pacer reservation after a cancelled wait.** `reserveStatsJobSlots` now re-checks the context
  after acquiring `writeMu`, as `pace` does, so a caller cancelled while queued for the lock
  reserves nothing instead of pushing every live writer back by a whole batch.
- **Flag wording.** The account-monitor row of `docs/api-catalog.md` still said
  `TWITTER_METRICS_ENABLED` gates only the monitor; it now says the flag gates both stats-job
  features (monitor and audience read) and excludes only the synchronous per-campaign metrics read.
- Tests: duplicate id with conflicting statuses and an unrequested id fail closed; a caller
  cancelled while waiting for the pacer lock leaves `nextWrite` unchanged. Each fails with its fix
  reverted.
