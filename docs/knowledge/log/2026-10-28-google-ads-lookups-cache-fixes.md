# 2026-10-28 — geo suggestion cache: empty answers and upstream metrics (PR #411)

**Fix** — a cached no-match geo search was copied out as a nil slice. A repeat of that search
therefore hit the orchestrator's nil-result guard and returned 503. Cache copies now use
`make`, so an empty answer stays `[]`. `GeoTargetSuggester.SuggestGeoTargets` now also returns
`cached`, and the orchestrator records a `suggest_geo_targets` upstream call only on a miss, so
cache hits no longer inflate Google traffic or skew the latency histogram. See
[google-ads-targeting-lookups](../architecture/google-ads-targeting-lookups.md).
