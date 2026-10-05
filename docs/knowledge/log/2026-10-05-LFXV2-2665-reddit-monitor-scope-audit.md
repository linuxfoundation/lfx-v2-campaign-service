# 2026-10-05 — LFXV2-2665 Reddit account-monitor scope audit (Track M3)

**Fix** — The Reddit half of the account-monitor scope-filter check (the
class of check that found Google's missing GAQL filters) has now been run.
**Outcome: no Google-class scope defect.** The read's only campaign filter,
`configured_status` ACTIVE/PAUSED, matches the BFF's `activeCampaigns`
filter exactly. The audit found five defects of other classes, all fixed:

1. **The window stopped as today began.** `ends_at` was rendered as today's
   `T00:00:00Z`, copied from the BFF, so a `days=N` read covered about N-1
   days while the account-monitor doc claimed Reddit followed the
   inclusive-of-today convention. Now rendered by `reportRange`, the same
   renderer `GetCampaignMetrics` uses (end bound at the final day's 23:00
   hour). Pinned with a fixed-clock test, including a mid-day and a
   non-UTC clock.
2. **The report used an unverified operation.** The per-campaign read
   POSTed to the BFF's nested `/ad_accounts/{a}/campaigns/{c}/reports`,
   which nothing in this repo verifies. It now uses
   `POST /ad_accounts/{a}/reports`, the operation verified against Reddit's
   published spec on LFXV2-3282, with `GetCampaignMetrics`' own body.
   Each row's `campaign_id` must match the requested campaign (provenance,
   via the now-shared `sumReportRows`), and all rows are summed — the old
   code read `metrics[0]` only and checked no id. One account report broken
   down by `CAMPAIGN_ID` was rejected because its completeness would depend
   on report pagination, whose fields this repo has not verified.
3. **The campaign list was never paginated.** `apiResponse` dropped the
   `pagination` envelope. The list and each report now follow
   `pagination.next_url` (same origin only) and fail loudly at a 50-page
   cap, on a repeated page, or on a duplicate campaign. `next_url` is
   Reddit's v3 convention and is **not** verified by this repo.
4. **goal_type was ignored.** A `DAILY_SPEND` campaign's daily cap was
   paced as a lifetime budget (false overspending). `LIFETIME_SPEND` →
   `TotalBudget`, `DAILY_SPEND` → `BudgetDay` (paced as `BudgetDay` × the
   window's days clipped to the flight, as LinkedIn's daily branch does),
   unknown or absent → neither, so the row is `PacingUnknown`.
5. **Two HIGH alerts for one condition.** A zero-delivery campaign raised
   both the zero-delivery item and "Underspending at 0%". The underspend
   item is now suppressed exactly when the zero-delivery item fired at 0%.
   That guard is deliberately narrower than the BFF's `pacingPct > 0`, which
   would also silence a CPC campaign with impressions but no clicks or spend.

Also fixed: the `decodeCampaignList` comment that cited the superseded
UNVERIFIED-CONTRACT banner. Deviations from the BFF kept on purpose: strict
`days` validation instead of clamping; totals as the row sum, excluding
archived/deleted campaigns; strict numeric typing of report rows; and the
campaign-id charset guard ahead of the report filter.

Still unverified, as before: no request has been made against a live Reddit
ad account, so whether `ends_at`'s final hour is inclusive and what the
pagination envelope looks like in practice remain open.
