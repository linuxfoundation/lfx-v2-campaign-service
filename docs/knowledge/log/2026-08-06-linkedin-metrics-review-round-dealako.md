# 2026-08-06 — LinkedIn metrics: untrusted values out of errors, UTC month boundaries, guard coverage

**Update** — dealako flagged that `costInUsdToMicros`'s parse failure interpolated the raw
`costInUsd` string into the returned error. That error propagates to
`BriefService.GetCampaignMetrics`'s default branch, which logs it, so an unvalidated field from an
upstream response body would land in the server log verbatim — the same pattern `1db44ee` removed
from the `adAnalytics` `apiError` earlier on this branch. The message is now value-free and carries
a comment recording why, so a future edit does not reintroduce the interpolation. The sibling count
guards were already value-free and needed no change.

**Update** — `MetricsWindowLastMonth` computed its month boundaries in `now.Location()`. Since `now`
was already UTC from `c.now().UTC()`, this had no behavioral effect, but the code was inconsistent:
every other window on this path explicitly uses `time.UTC`, so the inconsistency invited future
bugs. Changed to `time.UTC` for clarity and consistency with its siblings.

**Update** — `internal/dispatch/linkedin.go:296` formatted two `%w` verbs in one `fmt.Errorf`, which
works in Go 1.20+. Changed to `errors.Join(domain.ErrMetricsWindowUnsupported, err)` for clarity:
it explicitly names both errors as co-equal sentinels in the chain, avoiding the need for callers
to reason about `fmt.Errorf`'s wrapping behavior.

**Update** — Two coverage gaps in `costInUsdToMicros`, both for guards that existed but were
unpinned. `TestCostInUsdToMicros_NegativeValueRejected` covers the `Sign() < 0` branch.
`TestCostInUsdToMicros_RatioSyntaxRejected` covers `decimalCostPattern`, which matters more than it
looks: `big.Rat.SetString` accepts `"1/2"` as a fraction, so without the pattern check a response
carrying ratio syntax would parse to 0.5 rather than erroring. The same test pins the other syntaxes
`SetString` would otherwise tolerate or that whitespace would smuggle through — `1e3`, `0x10`,
` 1.5`, `1.5 `, `1,5`, and the empty string.

Both verified binding: neutering `decimalCostPattern.MatchString` and `Sign() < 0` fails each test
with `expected an error for ...` on every case.

**Update** — Swept Copilot's suppressed review-body findings, since an unresolved-thread count of
zero does not mean there is no open feedback. Most were already addressed by earlier rounds on this
branch: the campaign URN IS rebuilt from the bare persisted id (in the client, not the dispatcher),
the Ad Analytics GET DOES reuse the 429 retry policy and returns a terminal error rather than
`(nil, nil)` when the budget is exhausted, `last_month` no longer uses `AddDate(0, -1, 0)` (it
derives both boundaries from the first of this month, so March 31 cannot roll back into March), and
the concept files describe the real package boundary.

One was still live: nothing exercised `LinkedInDispatcher.ReadMetrics` beyond the unsupported-window
case, leaving the adapter boundary unpinned.
`TestLinkedIn_ReadMetrics_HappyPathBuildsURNsAndForwardsID` closes it — it asserts the BARE persisted
`PlatformCampaignID` reaches the client and gets wrapped into a `sponsoredCampaign` URN exactly once
(a double-wrap assertion guards the other direction), that the account URN comes from the resolved
connection rather than the campaign row, and that the decoded metrics survive the adapter intact.
`TestLinkedIn_ReadMetrics_PreflightErrorsNeverContactPlatform` covers the three guard branches with
a handler that fails the test if it is ever reached.

Both verified binding: replacing the URN build with a pass-through fails the first with
`campaigns=List(555)`, and neutering the account-id guard fails the third case with the client's
opaque `account ID is required` instead of the adapter's own diagnostic.

**Update** — Cursor caught that the previous round's sanitization was incomplete, and it was right.
`GetCampaignMetrics`'s own message was made value-free, but it wraps `costInUsdToMicros`'s error
with `%w`, and all four of that function's failure paths interpolated the input via `%q`/`%s`. The
untrusted value therefore still reached the log through
`BriefService.GetCampaignMetrics`'s default branch — the fix had been applied one layer above where
the decision actually gets made.

All four paths now report the failure reason plus the value's LENGTH instead of its bytes, which
keeps a malformed response diagnosable without reproducing it.
`TestCostInUsdToMicros_ErrorsNeverEchoTheValue` pins it using a marker string no legitimate
`costInUsd` can contain, so the absence of a leak is evidence rather than coincidence; restoring
any `%q` fails it.

**Update** — Cursor also flagged an unsynchronized capture in
`TestLinkedIn_ReadMetrics_HappyPathBuildsURNsAndForwardsID`: `gotQuery` was written on the httptest
handler's goroutine and read on the test's. The read happens after `ReadMetrics` returns, so it
never actually interleaved, but `-race` reasons about the absence of a happens-before edge rather
than observed timing, and the sibling test in `metrics_test.go` already uses the mutex pattern.
Guarded, and the value is copied out under the lock before the assertions.
