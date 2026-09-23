# 2026-09-17 monitor account endpoints — local review round 14

**Fix** — A fourteenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
three Important issues from `general`; `repo_code` and `repo_learnings` were
both clean.

1. Meta's `FetchFailed` guard (added in an earlier round to stop a
   never-measured campaign from getting a fabricated "underspending" action
   item) was inert in production: `internal/platform/meta/monitor.go`'s
   `fetchAccountCampaignInsights` dropped an unparseable insights row with a
   bare `continue`, leaving the campaign absent from `insightsByID` with no
   record of *why* — indistinguishable from a campaign that simply had zero
   delivery in the window. `ListAccountCampaigns` then left `FetchFailed` at
   its zero value for both cases, so the exact defect the branch documents as
   fixed still shipped for Meta specifically (Google has no partial-failure
   mode to trigger it; LinkedIn and Reddit already set the flag correctly).
   Fixed by having `fetchAccountCampaignInsights` return a second value — the
   set of campaign ids whose row it dropped for a parse failure — and having
   `ListAccountCampaigns` set `FetchFailed = true` for any campaign present in
   that set but absent from the successful-insights map. Added
   `TestListAccountCampaigns_MalformedInsightsRow_MarksFetchFailed` to pin it;
   the existing `TestEvaluateMetaMonitor_SkipsFetchFailedRows` only ever
   hand-constructed the flag, so it could not have caught this.
2. `fetchAccountCampaignInsights`'s dropped-row comment claimed the row is
   "`FetchFailed`-marked one layer up", which was untrue until fix #1 landed.
   Reworded to describe what the code actually does now (record the id in
   `failed`, let the caller mark it) so the comment stays true rather than
   aspirational.
3. Google's `ListAccountCampaignMetrics` computed its `[start, end]` window
   from the bare wall clock (`time.Now().UTC()`) in
   `internal/dispatch/googleads.go`, unlike LinkedIn's and Meta's equivalent
   windows, which this branch already moved onto each client's injected clock
   specifically so the window is deterministic and testable. Fixed by moving
   the window computation into `googleads.Client.ListAccountCampaigns` itself
   (mirroring the Meta/LinkedIn client-owned pattern exactly: the method now
   takes `days` instead of pre-computed date strings, and derives the window
   from `c.now()`). Added
   `TestListAccountCampaigns_UsesInjectedClockNotWallClock` in
   `internal/platform/googleads/monitor_test.go`, the sibling of the
   identically-named Meta/LinkedIn tests.

All three fixes bring Google and Meta's account-monitor code up to the same
standard LinkedIn and Reddit already met in earlier rounds, rather than
introducing a new pattern.
