# 2026-08-05 — GA-5: metrics-parse error handling and test race fix

**Update** — Follow-up fix on the GA-5 slice above, from its pre-PR local review cycle.
`internal/platform/googleads/metrics.go`'s `GetCampaignMetrics` was silently discarding
`strconv.ParseInt` errors on the three metrics fields (`impressions`/`clicks`/`costMicros`), so a
malformed upstream response would return a zero-value `CampaignMetrics` instead of surfacing a
decode failure — indistinguishable from a campaign that legitimately had no activity in the window.
Now returns a `*transportError` (same classification as the existing malformed-row JSON-decode
failure) when any of the three fields fails to parse, with a new
`TestGetCampaignMetrics_NonNumericMetricFieldIsTransportError` test. Also fixed the same
unsynchronized-`httptest`-handler-goroutine data race (see the GA-4 review-fixes entry) newly
introduced in `metrics_test.go`'s `TestGetCampaignMetrics_HappyPath` and
`TestGetCampaignMetrics_DefaultsWindowWhenEmpty` — both now guard their captured `gotBody` with a
`sync.Mutex`, matching the pattern already used in `internal/dispatch/googleads_test.go`.
