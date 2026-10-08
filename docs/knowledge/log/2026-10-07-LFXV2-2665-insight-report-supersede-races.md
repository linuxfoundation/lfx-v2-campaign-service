# 2026-10-07 — LFXV2-2665 stale-report supersede races closed

**Fix** — #294 review, `supersedeOtherPeriodPending` (shared by the Microsoft keyword and audience
reads):

- After losing the compare-and-set, the re-read pending report is re-validated: another
  old-period report (recorded by a request that read before the date change) is superseded in
  turn, at most `maxSupersedeAttempts` (3) times, instead of being adopted.
- The WHOLE re-read snapshot is adopted, ready half included, so a replacement that already
  finished is served instead of a second report being submitted.
- An unresolved supersession — a store error on the compare-and-set, a failed re-read, or the
  bound exhausted — now fails the read like any other saved-report store failure, instead of
  continuing with the stale snapshot (polling the obsolete report, submitting nothing, and
  answering `metrics_pending` for a period nothing is building for).
- The read-order doc comments and a test header now say a stale-period pending report is
  superseded before collection.
- Tests (#295 review) also cover a lost-CAS re-read that finds nothing saved for the key (treated
  as an empty key: exactly one current-period submission, `metrics_pending`), and the keyword
  read's lost-CAS re-read of another old-period report.

See [Microsoft keyword insights](../architecture/microsoft-keyword-insights.md).
