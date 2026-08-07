# 2026-08-06 — LinkedIn metrics: doc accuracy and count-guard coverage (LFXV2-2994)

**Update** — Corrected four documentation claims that no longer matched the code.
`internal-platform-linkedin.md` and the `GetCampaignMetrics` godoc both said the
platform-client method implements `service.MetricsReader`; it does not — the
signatures differ, and `dispatch.LinkedInDispatcher.ReadMetrics` is the
implementation. The same concept still described spend as a `float64` parse
rounded with `math.Round`, but `costInUsdToMicros` now parses exactly with
`big.Rat` and bounds-checks via `big.Int.IsInt64`. It also cross-referenced
`internal/platform/googleads/metrics.go`, which does not exist on this branch;
the reference is removed. `internal-dispatch.md` claimed all seven shared windows
map to a date range — only five do, with `yesterday` and `last_14_days` returning
`ErrUnsupportedWindow` (mapped to `domain.ErrMetricsWindowUnsupported`). The
`code/index.md` bullet was updated to match the concept's revised description.

**Update** — `dateRangeForWindow`'s godoc example asserted that an Asia/Tokyo
client on Jan 15 local time queries Jan 15 UTC. That is only true for part of the
day: the function calls `now().UTC()` before extracting the date, so an
early-morning local instant correctly resolves to the previous UTC day. Rewrote
the example to state the UTC-calendar contract precisely, and reworked
`TestDateRangeForWindow_TimezoneHandling`, whose comments claimed the opposite
(local-date preservation, "must NOT convert to UTC"). Its 10:30 UTC+10 instant was
Aug 5 in both zones and so passed either way; it now uses 06:00 UTC+10 — Aug 4 in
UTC — which fails if the normalization is removed.

**Update** — Added `TestGetCampaignMetrics_CountGuardsAreErrors`, a table-driven
test covering the negative-impressions/clicks rejection and both running-sum
overflow checks in the aggregation loop. Only the cost aggregate had coverage
before, so a regression dropping the count guards would have silently understated
or wrapped the two headline metrics on a 200 response. Each case was verified
binding by neutralizing its guard and observing the failure.
