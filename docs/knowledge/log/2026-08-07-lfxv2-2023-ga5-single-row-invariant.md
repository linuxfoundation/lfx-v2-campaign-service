# 2026-08-07 — Turning the GAQL single-row caveat into an enforced invariant

**Update** — Closed a review finding on PR #70 (`internal/platform/googleads/metrics.go`
and `metrics_test.go`).

`GetCampaignMetrics` carried a documented UNVERIFIED ASSUMPTION: a `segments.date`
WHERE-clause filter without `segments.date` in the SELECT list filters rows without
segmenting them, so the query returns at most one aggregated row. The code then read
`rows[0]` regardless of `len(rows)`. **A comment is not an invariant**, and this is the
class of assumption where the failure is silent: if a future SELECT reintroduces a
segmenting field, only the first segment's numbers survive and every metric is
UNDERREPORTED — which is indistinguishable from a genuinely quiet campaign, so nothing
downstream can detect it.

`len(rows) > 1` is now an error naming the count it saw.

**Summing the rows instead would be worse, not safer.** It looks like the tolerant
choice, but the same query change that produces multiple rows also invalidates
`CampaignMetrics.Window`: each row would then cover a sub-window, and a summed result
would claim a window it does not describe. Papering over a broken query with a
plausible-looking number is the outcome with no recovery path; failing loudly is the
only one an operator can act on.

`TestGetCampaignMetrics_MultipleRowsIsAnErrorNotAPartialSum` drives a two-row response
totalling 3000 impressions and asserts the call fails with a nil result and a diagnostic
carrying the row count. Revert-verified — with the guard removed it fails reporting
`Impressions:1000`, the exact 3x underreport the guard exists to prevent.

Separately flagged in the same review and deliberately NOT fixed here: `resolveGoogleAdsClient`
builds a fresh client (and OAuth exchange) per call, with no cache keyed on
project/connection. That is a real cost under dashboard polling, but the fix is a
dispatcher-scoped cache with credential-change invalidation — its own change, in a PR
that is already over the hand-written line cap. Tracked as a follow-up.
