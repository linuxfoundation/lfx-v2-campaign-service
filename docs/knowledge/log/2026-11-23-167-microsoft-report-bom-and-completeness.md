# 2026-11-23 — #167 Microsoft reports: byte-order mark and completeness flag

**Fix** — The first live run of `cmd/verify-metrics` for `MICROSOFT_METRICS_ENABLED` (dev cluster,
LF system account 138011001, campaign 568362105) found two blockers. Both affect every read
behind the flag, the account monitor included.

1. The report CSV opens with a UTF-8 byte-order mark, and `encoding/csv` refused every file with
   `bare " in non-quoted-field`. `downloadReportRecords` now strips it.
2. With `ReturnOnlyCompleteData=false`, Microsoft sets "Potential Incomplete Data: true" on every
   report, even `last_month`, so the per-campaign read refused every window. Now a settled window
   (`windowIsSettled`, ending at least two days before today) is requested with `true`, which
   returned the flag `false` live, and the flag is held only for those reads. Other windows read
   as in progress. `true` on a window that includes today fails with
   `NoCompleteDataAvaliable` (2004).

Re-run with both fixes: `last_30_days` read 20,569 / 953 / $2,053.03, consistent with Fivetran
plus a partial current day, and `last_month` read 34,887 / 973 / $1,984.37.
