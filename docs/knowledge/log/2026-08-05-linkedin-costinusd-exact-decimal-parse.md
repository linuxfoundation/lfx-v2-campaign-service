# 2026-08-05 — LinkedIn costInUsd: parse exact decimal, not float64

**Fix** — Addressed a Copilot finding on `costInUsdToMicros`
(`internal/platform/linkedin/metrics.go`). LinkedIn's Ad Analytics API returns
`costInUsd` as a BigDecimal serialized as a JSON string. The prior
implementation parsed it via `strconv.ParseFloat` before multiplying by
1,000,000 and rounding — but float64 only carries ~15-17 significant decimal
digits, and at magnitudes near the upper end of the valid range (e.g.
`9007199254.740993` USD) the representable-value spacing exceeds 1 micro, so
the float64 intermediate can silently misrepresent the exact odd micros value
before the overflow check even runs.

Replaced the float64 path with `big.Rat`, which parses the decimal string
exactly and keeps the multiplication by 1,000,000 exact; `FloatString(0)` then
rounds to the nearest whole micro (ties away from zero), the same rounding
behavior as before, but as the only intentional precision loss in the
pipeline rather than an incidental one. Overflow is checked via
`big.Int.IsInt64()` on the exact result instead of comparing against a
float64-promoted `math.MaxInt64`.

This also flips the old float64-representability boundary test: a value
converting to exactly `math.MaxInt64` micros (`9223372036854.775807`) now
correctly succeeds (it fits) rather than being rejected as a float64-rounding
artifact; a value one micro beyond now correctly fails.
