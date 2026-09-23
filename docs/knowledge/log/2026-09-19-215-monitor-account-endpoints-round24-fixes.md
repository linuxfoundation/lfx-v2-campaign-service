# 2026-09-19 monitor account endpoints — round 24 review fixes

**Fix** — Verified all 18 threads GitHub still reported unresolved on PR #215
against current code before writing anything (per standing guidance against
one-finding-at-a-time review loops): 15 were already fixed by earlier rounds
but never marked resolved on the thread; the remaining 3 genuinely open
Copilot findings plus a human reviewer's `fetchFailedRow`-duplication nit
are fixed here.

1. `internal/platform/googleads/monitor.go`'s `ListAccountCampaigns` fed a
   present-but-unparseable `campaign_budget.amount_micros` through
   `microsToUSD`, which returned a bare `0` on any parse error — silently
   converting malformed upstream data into a trusted real `$0` daily budget,
   with no `FetchFailed` signal. `microsToUSD` now returns `(usd float64, ok
   bool)`: an empty string or Google's `-1` no-budget-set sentinel are a
   legitimate zero (`ok=true`); anything else that fails to parse is
   `ok=false`, and the call site marks the row `FetchFailed=true` rather than
   keeping the fabricated `$0`. New tests
   `TestListAccountCampaigns_MalformedBudget_MarksFetchFailed` and
   `TestMicrosToUSD_EmptyAndSentinelAreNotFailures` pin both the failure case
   and the two legitimate-zero cases it must not be confused with.
2. `internal/platform/linkedin/monitor.go`'s `fetchAccountCampaignList` page
   loop treated an ABSENT `metadata` block on the adCampaigns response the
   same as an empty `NextPageToken` — "no more pages" — so a malformed or
   truncated intermediate page silently returned a partial campaign list as
   a complete one. `accounts.go`'s adAccount picker already rejects this
   exact state (`accounts.go:202-211`); the campaign-list walk did not. Now
   returns an error when `metadata` is nil before checking
   `NextPageToken`. New test
   `TestListAccountCampaigns_MissingCampaignListMetadata_IsRejected` pins it.
3. `internal/platform/linkedin/monitor.go`'s
   `fetchAccountCampaignAnalyticsRaw` decoded `elements` as a value-typed
   `[]struct{...}`, so `{}`, `"elements":null`, and a missing field all
   decoded identically to an empty/nil slice with no error — indistinguishable
   from a genuine empty array. `ListAccountCampaigns` would then read every
   listed campaign as measured zero activity instead of a failed analytics
   read, fabricating a false "no delivery" pacing/action-item verdict from
   data that was never actually fetched. `Elements` is now `*[]struct{...}`;
   a nil pointer after unmarshal is rejected with an explicit error, while a
   genuine `[]` still decodes to a non-nil pointer to an empty slice. New
   test `TestListAccountCampaigns_NullAnalyticsElements_IsRejected` (both
   `null elements` and `absent elements` subtests) pins it.
4. `internal/service/rules/monitor_reddit.go`'s `EvaluateRedditMonitor`
   hand-duplicated `fetchFailedRow`'s body (`m.PacingUnknown = true; out =
   append(...); items = append(...)`) in the `m.StartDate == ""` branch added
   by round 23, instead of calling the shared helper directly — a human
   reviewer's nit. Replaced with `row := fetchFailedRow(m); out = append(out,
   row); items = append(items, redditActionItems(row.Metrics, 0)...)`,
   behavior-preserving (`fetchFailedRow` already does exactly this).
   Existing `TestEvaluateRedditMonitor_EmptyStartDate_SetsPacingUnknown`
   continues to pass unchanged.

Verification: `go build ./...`, `go vet ./internal/service/rules/...`, and
`go test ./internal/platform/googleads/... ./internal/platform/linkedin/...
./internal/service/rules/... -v` all clean/passing.
