# 2026-08-07 — LFXV2-2993 Meta metrics currency and overflow-guard accuracy

**Update** — Two descriptions were wrong about what the code does, though the code
itself is correct.

`docs/knowledge/code/internal-dispatch.md` called Meta's `spend` a decimal-USD
string. The client and the `meta` package both define it as whole units of the ad
ACCOUNT's currency, and the service performs no FX conversion, so a non-USD account
was documented with the wrong currency semantics. Corrected to account-currency.

The overflow-guard rationale in `internal/platform/meta/metrics.go` (and the matching
test comment) attributed the guard to rounding: it claimed a product in
`[2^63-0.5, 2^63)` would round up to `2^63` and slip past a `>` comparison. Float64
spacing at that magnitude is 2048, so no such value is representable and rounding
plays no part. The real reason the guard works is that `math.MaxInt64` is not exactly
representable, `float64(math.MaxInt64)` is `2^63` — one MORE than `MaxInt64` — and a
product of exactly `2^63` therefore passes `>` and wraps to `MinInt64` on conversion.
Only `>=` rejects it. `math.Round` is retained for its actual purpose: rounding rather
than truncating sub-boundary spends.

`TestGetCampaignMetrics_SpendAtInt64BoundaryOverflows` binds this: changing `>=` back
to `>` fails it.
