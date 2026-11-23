# 2026-11-23 — #167 Reddit report rows need the CAMPAIGN_ID breakdown

**Fix** — The first live run of `cmd/verify-metrics` for `REDDIT_METRICS_ENABLED`, done in the
dev cluster against the LF system Reddit account, failed with `row is missing or null field(s)
campaign_id`. With a `campaign:id==<id>` filter and no `breakdowns`, Reddit returns the window
as one aggregated row without `campaign_id`, even though `CAMPAIGN_ID` is in `fields`. That
row fails the provenance check in `sumReportRows`. The ungated account monitor sends the same
body (`fetchMonitorCampaignReport`), so since v1.0.40 (#660) every prod Reddit Monitor
campaign read fails the same way.

Both reads now build the body with `campaignReportBody`, which also sends `"breakdowns":
["CAMPAIGN_ID"]`. Re-run with the fix, campaign 2577406841688564185 over `last_30_days`
read 505,751 impressions, 1,365 clicks and $8,190.46. Reddit's reporting via Fivetran gave
$8,190.45 for the same days; the cent comes from per-day rounding. An inactive campaign
returns an empty `metrics` array. The match lines up with account-time (America/New_York)
days, not UTC.
