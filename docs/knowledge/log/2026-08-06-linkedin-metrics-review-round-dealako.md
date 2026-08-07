# 2026-08-06 — LinkedIn metrics: untrusted values out of errors, UTC month boundaries, guard coverage

**Update** — dealako flagged that `costInUsdToMicros`'s parse failure interpolated the raw
`costInUsd` string into the returned error. That error propagates to
`BriefService.GetCampaignMetrics`'s default branch, which logs it, so an unvalidated field from an
upstream response body would land in the server log verbatim — the same pattern `1db44ee` removed
from the `adAnalytics` `apiError` earlier on this branch. The message is now value-free and carries
a comment recording why, so a future edit does not reintroduce the interpolation. The sibling count
guards were already value-free and needed no change.

**Update** — `MetricsWindowLastMonth` computed its month boundaries in `now.Location()`. Every other
window on this path is UTC-anchored, so a non-UTC process TZ would have shifted only this one
window's start/end by the offset, silently including or excluding a day at each edge. Now
`time.UTC`, matching its siblings.

**Update** — `internal/dispatch/linkedin.go:296` formatted two `%w` verbs in one `fmt.Errorf`. Go
wraps only the first, so `errors.Is(err, domain.ErrMetricsWindowUnsupported)` held while the
underlying client error was unwrappable — the caller could see the sentinel but never the cause.
Replaced with `errors.Join(domain.ErrMetricsWindowUnsupported, err)` under a single `%w`, which
keeps both reachable.

**Update** — Two coverage gaps in `costInUsdToMicros`, both for guards that existed but were
unpinned. `TestCostInUsdToMicros_NegativeValueRejected` covers the `Sign() < 0` branch.
`TestCostInUsdToMicros_RatioSyntaxRejected` covers `decimalCostPattern`, which matters more than it
looks: `big.Rat.SetString` accepts `"1/2"` as a fraction, so without the pattern check a response
carrying ratio syntax would parse to 0.5 rather than erroring. The same test pins the other syntaxes
`SetString` would otherwise tolerate or that whitespace would smuggle through — `1e3`, `0x10`,
` 1.5`, `1.5 `, `1,5`, and the empty string.

Both verified binding: neutering `decimalCostPattern.MatchString` and `Sign() < 0` fails each test
with `expected an error for ...` on every case.
