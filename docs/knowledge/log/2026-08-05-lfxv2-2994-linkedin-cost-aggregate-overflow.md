# 2026-08-05 — LFXV2-2994: LinkedIn metrics cost aggregate overflow

**Update** — Addressed a review finding (Copilot) against
`internal/platform/linkedin/metrics.go`'s `GetCampaignMetrics`. Each element's
`costInUsd` was converted to micros via `costInUsdToMicros`, which already rejects
a single value that would overflow `int64` — but the per-element `metrics.CostMicros
+= micros` loop summed those individually-valid values with no check on the running
total. Two or more large elements could wrap the aggregate negative, returning a
200 with understated (or negative) spend rather than surfacing the overflow. Added
a bounds check (`micros > math.MaxInt64-metrics.CostMicros`) before each addition
that rejects the read instead of wrapping, plus a test with two elements whose
individually-valid costs overflow only when summed — verified it fails with the
exact wrapped-negative symptom when the fix is reverted.
