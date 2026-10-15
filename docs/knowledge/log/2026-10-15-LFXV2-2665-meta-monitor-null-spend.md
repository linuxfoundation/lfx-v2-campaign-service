# 2026-10-15 — An explicit null Meta Insights metric is unreadable, not zero (LFXV2-2665, #316)

**Fix** — The Meta account monitor's campaign-level Insights read decoded `impressions`,
`clicks` and `spend` into string fields, so an explicit `"spend": null` read as `""` — the same
as an omitted spend — and was accepted as a measured zero. With the ad-set budget fallback
supplying a usable budget, that campaign paced at 0% and raised a false Underspending item
instead of being `FetchFailed`. `fetchAccountCampaignInsights` now decodes the three metrics as
`json.RawMessage`: counters go through `rawCounter` (as the ad-set Insights read in `ad_sets.go`
already did) and spend through the new `rawMonitorSpend`; ABSENT (or an empty string) is still
zero, while an explicit `null` or a non-string value marks only that campaign `FetchFailed`
(class `decode`). `GetCampaignMetrics` (`metrics.go`) had the same hole and now reads its three
metrics through `rawCounter` too, failing the read on a null. Tests:
`TestListAccountCampaigns_NullInsightsMetricIsFetchFailed` (null spend/impressions/clicks and a
numeric spend fail only that campaign, the other is untouched, `EvaluateMetaMonitor` emits no
action item; omitted and empty spend stay zero) and `TestGetCampaignMetrics_NullMetricIsAnError`.
No API-shape or migration change. See `code/internal-platform-meta.md`.
