# 2026-10-07 — LFXV2-2665 X audience read: pre-PR review fixes

**Fix** — Fixes from the pre-PR review of the X audience read (`get-twitter-ads-audience`):

- **Flag gate before the scope.** `TWITTER_METRICS_ENABLED` was checked only in the dispatcher,
  but `readScopedAudience` answers an empty scope without reaching it, so a disabled read gave
  200 `[]` to projects with no X campaigns. `TwitterAudienceReader` gained `AudienceEnabled()`,
  which `Orchestrator.ReadTwitterAudienceInsights` calls first: off is 400 for every project.
- **Bounded stats jobs per shared account.** Each read creates up to six X stats jobs, X allows
  100 concurrent per ACCOUNT, the account is shared by every foundation on it and by the X account
  monitor, and a timed-out read's jobs keep running. New `twitterAudienceGuard` (process-wide, per
  dispatcher): identical concurrent reads share one set of jobs; successful results are cached for
  5 minutes (256 entries, keyed by account + window + sorted scope, valid only while the window
  still names the same account-local instants); one read per account at a time, a waiter giving
  up with its context (503). `statsJobsFitBudget` now counts the write pacer's backlog, so a read
  that cannot finish its job POSTs in time refuses before the first one (this also applies to the
  account monitor's submission).
- **Window wording.** The read shares the X metrics read's window NAMES, not its instants: it uses
  account-local days, the metrics read UTC days. The doc comment, the Goa description, the
  api-catalog row and the concepts no longer call the two comparable.
- **All-null segmented files.** X's forum reports segmented jobs whose files carry only null
  metrics; this package already reads X's null as "no activity" and X does not document idle
  entities as omitted, so the two cannot be told apart and failing closed would 503 every idle
  project. Chose a new required envelope boolean `all_counters_null` (true when any dimension's
  rows carried no measured counter) over a 503.
- **Tests.** A partially finished job set at the deadline fails whole with no download; the
  results-file stub compresses without `t.Fatalf` on the handler goroutine; tests for each guard
  arm, the pacer backlog and the flag. Each new test was checked to fail with its fix reverted.
