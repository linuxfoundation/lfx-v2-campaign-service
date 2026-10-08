# 2026-10-07 — LFXV2-2665 HubSpot monitor pre-PR review fixes

**Fix** — Pre-PR review of `monitor-hubspot-account`.

- **Connection.** The dispatcher now resolves like Dispatch and ReadMetrics (own connection, else
  the LF system row) instead of own-only: the read never widens past the project's recorded email
  ids and the per-email portal check is the boundary, so own-only 404'd projects whose emails went
  out through the LF portal. A row recorded against another portal is still unattributable.
- **Findings join rows.** HubSpot findings carry `campaign_id` = the service campaign UUID (as on
  the rows) plus a new optional `email_id` on the shared `AccountMonitorActionItem` (absent on the
  ad monitors, so backward compatible).
- **Spam rule.** Also needs ≥3 reports for MED and ≥5 for HIGH; one complaint on 100–333
  deliveries no longer fires HIGH.
- **Throttling.** Concurrency 4 → 2, no pacer (the client has none); the read is bounded at 101
  requests and a 429 outlasting retries is a 503 with no partial result — now documented.
- **Raw check scope.** Statistics responses use `identityjson.CheckExactKeys` plus
  `FoldedKeyCollision` on the struct-decoded levels, so a case-distinct pair in an open map
  (`deviceBreakdown`) no longer 503s the per-campaign read; exact duplicates still do. The
  campaign-metrics catalog row notes the new HubSpot 503 cases.
- **`spam_rate`** on rows and totals.
- **`emails_truncated`** only when the oldest checked row was recorded on or after the window
  start (residual: an older draft sent late is not flagged).
- The creation log entry was re-dated to 2026-10-07.
