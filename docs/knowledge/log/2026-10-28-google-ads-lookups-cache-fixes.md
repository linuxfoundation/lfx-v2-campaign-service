# 2026-10-28 — geo suggestion cache: empty answers and upstream metrics (PR #411)

**Fix** — a cached no-match geo search was copied out as a nil slice. A repeat of that search
therefore hit the orchestrator's nil-result guard and returned 503. Cache copies now use
`make`, so an empty answer stays `[]`. A cache hit is no longer recorded as a
`suggest_geo_targets` upstream call, so it does not inflate Google traffic or skew the latency
histogram (the `called` return that decides this is described in the next entry). See
[google-ads-targeting-lookups](../architecture/google-ads-targeting-lookups.md).
