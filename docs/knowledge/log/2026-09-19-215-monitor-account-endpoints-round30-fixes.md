# 2026-09-19 monitor account endpoints — round 30 review fixes

**Fix** — Five of the remaining unresolved GitHub review threads on PR #215
(distinct from the local Pi/Claude review trio rounds 24-29 above) named real
defects; fixed here rather than ported/left open, following the same
false-empty/false-zero principle already applied throughout this feature.

1. **Reddit.** `internal/platform/reddit/monitor.go`'s `decodeCampaignList`
   returned a plain `[]campaignElement` and treated any shape it didn't
   recognize the same as a legitimately empty list, so a malformed/
   unrecognized campaign-list response silently became "this account has
   zero campaigns" — a false-empty result, the same failure class
   `fetchMonitorReport`'s own malformed-JSON branch in this file already
   refuses to reproduce. Changed the signature to
   `(elements []campaignElement, ok bool)`: an absent/empty body is still a
   legitimate empty account (`ok=true`, matching Reddit's own convention for
   "no campaigns"), but a body that matches neither the bare-array nor the
   `{"campaigns":[...]}` shape now returns `ok=false`, and
   `ListAccountCampaigns` surfaces that as an error (routed through
   `classifyDiscoveryError`'s default 503 arm via `internal/dispatch/
   reddit.go`'s unwrapped propagation) instead of an empty, misleadingly
   "successful" result. New tests
   `TestListAccountCampaigns_MalformedCampaignListShape_ReturnsError` and
   `TestListAccountCampaigns_EmptyCampaignListBody_ReturnsNoRowsWithoutError`
   pin both halves.
2. **LinkedIn.** `internal/platform/linkedin/monitor.go`'s `parseUSDAmount`
   swallowed a `strconv.ParseFloat` error and returned a trusted `0`,
   converting a non-empty, unparseable `dailyBudget`/`totalBudget` amount
   into a fabricated real zero budget — the same false-zero class the
   sibling `costInUsd` fix (round-19) and the Google Ads/Meta budget fixes
   (rounds 24/25) already cover, just never applied to this function.
   Changed to `(amount float64, ok bool)` — empty string is still a
   legitimate zero (LinkedIn omits the field rather than sending `"0.00"`),
   a non-empty unparseable value now sets `ok=false` and the call site marks
   `AccountCampaignRow.FetchFailed=true` instead of trusting the `0`.
   Fixing this also surfaced a second, previously-latent bug in
   `ListAccountCampaigns`: it unconditionally overwrote
   `row.FetchFailed = m.FetchFailed` when a campaign had an analytics row,
   which would have silently cleared a genuine budget-parse failure the
   moment the campaign also had a successful metrics read. Now ORs the two
   signals (`row.FetchFailed = row.FetchFailed || m.FetchFailed`) rather
   than overwriting. New tests
   `TestListAccountCampaigns_MalformedDailyBudget_MarksFetchFailed` (also
   asserts the OR-fix: a malformed budget must not discard a successful
   analytics read) and `TestListAccountCampaigns_EmptyBudgetAmount_IsZeroNotFailed`.
3. **Meta.** `internal/platform/meta/monitor.go`'s `minorUnitsToWhole` had
   the identical false-zero defect as LinkedIn's `parseUSDAmount` — a
   non-empty, unparseable `daily_budget`/`lifetime_budget` minor-units string
   silently became a trusted `$0`. Same fix shape: `(whole float64, ok
   bool)`, with the new `metaCampaignListEntry.FetchFailed` field and
   `ListAccountCampaigns` propagating it onto `AccountCampaignRow.FetchFailed`
   (unlike LinkedIn, Meta's campaign-list and insights reads populate
   disjoint parts of the row before any OR is needed, so this one is a
   plain assignment, not an overwrite hazard). New tests
   `TestListAccountCampaigns_MalformedDailyBudget_MarksFetchFailed` and
   `TestListAccountCampaigns_EmptyBudget_IsLegitimateZero`.
4. **Meta.** The same file's `dateOnly` truncated a timestamp to its first
   10 characters (`s[:10]`) without checking those characters formed an
   actual calendar date, so a malformed `start_time`/`stop_time` (e.g. a
   zeroed or garbled timestamp) would pass through as a fabricated-looking
   date instead of the `""` the function's own doc comment already promised
   for malformed input. Now parses the prefix with
   `time.Parse("2006-01-02", ...)` and returns `""` on a parse error. New
   table test `TestDateOnly` pins valid, too-short, empty, non-calendar
   (`"0000-00-00..."`), and non-numeric-prefix cases.
5. **Shared.** `sortByPriority` (`internal/service/rules/monitor_google.go`,
   shared by all four `monitor_*.go` rule-engine files) ran a hand-written
   O(n²) insertion sort. Replaced with `sort.SliceStable`, which preserves
   the same stability guarantee (mirroring `Array.prototype.sort`'s
   stability the BFF this was ported from relies on) in O(n log n) instead —
   a pure efficiency fix with no output change, so no new test was needed
   beyond the existing per-platform `sortByPriority`-exercising tests in
   `monitor_google_test.go`/`monitor_linkedin_test.go`/
   `monitor_reddit_test.go` continuing to pass unchanged.

Verified: `gofmt -l .` clean, `go vet ./...` clean, `go build ./...` clean,
`go test ./...` green across the whole repo.
