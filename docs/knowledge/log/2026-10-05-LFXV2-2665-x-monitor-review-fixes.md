# 2026-10-05 — X account monitor: PR #252 review fixes

**Fix** — five review findings on the X Ads account monitor, each fixed where the monitor would
otherwise have reported something untrue.

- **Gaps between line items are not scheduled days.** The flight was folded into one
  earliest-start/latest-end envelope, so line items Sep 1–5 and Oct 1–5 made a Sep 15–21
  window look scheduled: a false zero-delivery HIGH and pacing over days nothing could serve.
  The X client now also returns the UNION of the line items (`AccountCampaign.Flights`,
  carried as `model.AccountCampaignMetrics.FlightRanges`), merging overlapping or touching line
  items and keeping disjoint ones apart, and `rules.EvaluateTwitterMonitor` counts covered and
  scheduled days from the ranges. A daily budget is paced over the scheduled days of the
  window, and a total budget is prorated over the scheduled days with the gaps left out.
- **The window queried is the window saved.** `accountReportWindow` used to floor bounds to
  the hour for fractional-offset zones and trim an hour off a 90-day window across a DST
  fall-back, so the saved first/last day could differ from what X was asked for. Now a local
  midnight off the whole UTC hour (Asia/Kolkata …) is refused before any stats request
  (`twitter.ErrReportWindowNotWholeHours` → `domain.ErrAccountTimezoneUnsupported` → 409,
  reason `account_timezone_unsupported`), and a 90-day fall-back window drops its earliest
  local day. It then covers the trailing 89 whole days, and the saved window says so.
- **More than 200 active campaigns is a distinct, documented 409.** It used to fail with a
  generic error that was logged and retried on every read.
  `twitter.ErrTooManyActiveCampaigns` → `domain.ErrAccountTooManyActiveCampaigns` → 409,
  reason `account_too_many_active_campaigns`. Both permanent refusals now fail the read
  (`Orchestrator.ReadReportedAccountCampaigns`, `isPermanentReportRefusal`) and are mapped by
  `classifyDiscoveryError`. `monitor-twitter-ads-account` declares `Conflict`, and the
  `conflict-error` reason enum gained both values (design change, regenerated).
- **No currency symbol on X amounts.** They are in the account's own currency, and the
  account resource does not carry its code. Item text now reads "12.50 in account currency",
  and the placeholder-budget remedy no longer quotes "$10-50/day".
- **`metrics_as_of` / `metrics_pending` docs say Microsoft and X**, not Microsoft only.

Concepts updated: `architecture/account-monitor-endpoints.md`,
`code/internal-platform-twitter.md`, `code/internal-service-rules.md`,
`code/internal-service.md`; also `docs/api-catalog.md`.
