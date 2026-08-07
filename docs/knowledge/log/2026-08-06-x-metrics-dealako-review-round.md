# 2026-08-06 — X metrics: dealako review round (LFXV2-2996)

**Fix** — `GetCampaignMetrics` (`internal/platform/twitter/metrics.go`) discarded the
`time.Parse` error when re-parsing `dateRangeForWindow`'s `endDate` to derive the
exclusive next-midnight `end_time` bound. The error is unreachable today because
`dateRangeForWindow` produces its output via `Format("2006-01-02")`, but nothing
enforces that invariant across a future refactor of it, and a swallowed parse failure
would silently yield a zero-time `end_time` that under-reports every window. Now
returns a wrapped error.

**Fix** — Added `TestGetCampaignMetrics_Last7DaysQueryParams`
(`internal/platform/twitter/metrics_test.go`), pinning the full
`dateRangeForWindow` → query-string chain for `LAST_7_DAYS` against a fixed clock.
The existing coverage checked each link in isolation — `TestDateRangeForWindow_Last7Days`
does the date math with no HTTP request, `TestGetCampaignMetricsHappyPath` asserts the
non-date params, and `TestGetCampaignMetrics_EndTimeIsHourAligned` covers only `TODAY` —
so a regression in the `LAST_7_DAYS` wiring specifically could pass all three. The new
test asserts `start_time=2025-01-09T00:00:00Z` and `end_time=2025-01-16T00:00:00Z`,
documenting that `LAST_7_DAYS` spans the 7 calendar days INCLUDING today (a 7-day range
under the exclusive end bound, not 8). Verified binding by shifting the `AddDate` offset
and observing the test fail.
