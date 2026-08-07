# 2026-08-06 — LFXV2-2994: fix Copilot findings on LinkedIn metrics aggregation

**Update** — `GetCampaignMetrics` (`internal/platform/linkedin/metrics.go`) only
guarded the `CostMicros` running sum against `int64` overflow. Two
individually valid `int64` `impressions`/`clicks` values from separate
response elements could still overflow their running sums, silently
returning negative counts and an invalid `Ctr` in an otherwise-successful
response. Added the same negative-value and overflow-before-add guards
already used for `CostMicros` to the `Impressions`/`Clicks` accumulation.

**Update** — `costInUsdToMicros` parsed `costInUsd` via `big.Rat.SetString`,
which accepts rational syntax (e.g. `"1/2"`) in addition to plain decimals.
A malformed value like `"1/2"` would silently parse as `500,000` micros
instead of being rejected, violating the function's documented "clean
decimal" contract. Added a `decimalCostPattern` regexp
(`^-?[0-9]+(\.[0-9]+)?$`) that the raw string must match before it's handed
to `big.Rat.SetString`.

Both are Copilot findings from PR #73's review round.
