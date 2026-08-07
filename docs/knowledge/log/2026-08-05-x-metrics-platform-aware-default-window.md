# 2026-08-05 — Platform-aware default window for GetCampaignMetrics

**Update** — Addressed a Mandatory Copilot finding on `GetCampaignMetrics`
(`internal/service/brief.go`). The omitted-`window` default was a single
global constant, `model.MetricsWindowLast30Days`. X Ads' stats endpoint caps
queryable date ranges at 7 days per request (`internal/platform/twitter`,
`internal/dispatch/twitter.go`'s `twitterMetricsWindow` only maps `today` and
`last_7_days`), so `last_30_days` is unreachable for that platform — every
omitted-window metrics request against an X campaign would fail with a
guaranteed 400.

Added `defaultMetricsWindowFor(platform model.Provider) model.MetricsWindow`,
switching on platform: `last_7_days` for `ProviderTwitterAds`, `last_30_days`
for everything else. Updated the published contract (`design/brief.go`'s
`window` attribute description, regenerated via `make apigen`), the
per-platform default note in `docs/knowledge/code/internal-service.md`, and
the `window` query-param cell in `docs/api-catalog.md`. Added two tests
(`TestBriefService_GetCampaignMetrics_DefaultWindowIsLast30Days`,
`TestBriefService_GetCampaignMetrics_DefaultWindowIsPlatformAwareForTwitter`)
covering the happy-path default resolution for both branches.
