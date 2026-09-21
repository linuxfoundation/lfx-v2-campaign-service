# 2026-09-17 monitor account endpoints — local review round 19

**Fix** — A nineteenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, ran
alongside a new automated Copilot PR review that landed independently on the
same commit. Both converged on the same defect class across all four
platform dispatchers — a swallowed or mis-modeled per-row failure silently
read as a genuine zero-delivery measurement — plus one Should-fix
documentation gap from `repo_code`.

1. (`general`, corroborated by Copilot's own `meta/monitor.go:287` finding)
   `fetchAccountCampaignInsights` (`internal/platform/meta/monitor.go`)
   unconditionally called `strconv.ParseFloat(row.Spend, 64)`. Meta omits
   `spend` entirely (empty string) for a genuinely zero-delivery campaign
   rather than sending `"0"` — the same convention `parseMetricInt` already
   honors for `impressions`/`clicks` — so an unconditional parse would have
   marked every legitimate zero-spend row `FetchFailed`. Fixed to only
   attempt the parse when `row.Spend != ""`; a non-empty unparseable value
   still marks the row failed via the existing `failed` set. Added
   `TestListAccountCampaigns_MalformedSpend_MarksFetchFailed` and
   `TestListAccountCampaigns_EmptySpend_IsLegitimateZero` to pin both the
   fix and the zero-spend convention it must not regress.
2. (Copilot, `googleads/monitor.go:134`) `ListAccountCampaigns`
   (`internal/platform/googleads/monitor.go`) skipped a campaign row whose
   GAQL `impressions`/`clicks`/`costMicros` failed to parse without ever
   setting a failure marker anywhere in the package — the row's zero-valued
   accumulator then read to the rule engine as a real "no delivery"
   measurement. Added `FetchFailed bool` to `AccountCampaignRow`, set it on
   a metrics-parse failure, wired it through `internal/dispatch/googleads.go`
   into `model.AccountCampaignMetrics`, and updated the stale comment above
   `monitor_google.go`'s `if m.FetchFailed` check that had claimed nothing in
   this package ever set the field. Added
   `TestListAccountCampaigns_MalformedMetrics_MarksFetchFailed`.
3. (Copilot, `linkedin/monitor.go:93`) `ListAccountCampaigns`
   (`internal/platform/linkedin/monitor.go`) marked every campaign absent
   from the account-wide analytics pivot response `FetchFailed=true`. That
   response is one account-wide call that either returns the whole metrics
   map or a non-nil error — LinkedIn omits a campaign with no activity in
   the window from the pivoted result entirely, so absence in a
   SUCCESSFUL response means legitimate zero, not a failed fetch. Fixed to
   default an absent campaign's row to its zero values, `FetchFailed=false`.
   Added `TestListAccountCampaigns_CampaignAbsentFromAnalytics_IsZeroNotFailed`.
4. (Copilot, `linkedin/monitor.go:266`) The same file's
   `fetchAccountCampaignAnalyticsRaw` silently ignored a non-empty,
   unparseable `costInUsd`, leaving `SpendUSD` at its zero default — an
   upstream-data failure reported as a trusted zero spend. Added
   `FetchFailed bool` to `monitorMetricsRow` and set it when
   `costInUsdToMicros` errors on a non-nil `costInUsd`, propagated through
   to the row in `ListAccountCampaigns`. Added
   `TestListAccountCampaigns_MalformedCostInUsd_MarksFetchFailed`.
5. (Copilot, `reddit/monitor.go:228`) `ListAccountCampaigns`
   (`internal/platform/reddit/monitor.go`) issued one HTTP `/reports` call
   per active campaign serially, inside the read's own fixed caller
   deadline — an account with several campaigns could time out even with
   every individual request healthy. Rewrote the per-campaign loop to use
   the same bounded-concurrency `errgroup.WithContext` + `SetLimit` pattern
   already established in `internal/service/brief.go` and
   `internal/service/orchestrator.go` (new `monitorReportConcurrency = 5`,
   mirroring the ported BFF's own batch size), writing into a preallocated
   `rows[i]` slice by index and never propagating a per-campaign error out
   of `g.Go` — a per-campaign failure already has a home in that row's
   `FetchFailed`. Added `internal/platform/reddit/monitor_test.go` (new
   file for this package): `TestListAccountCampaigns_BoundsReportConcurrency`
   asserts the fan-out completes well under the serial worst case and that
   no more than `monitorReportConcurrency` report requests are ever in
   flight at once, and
   `TestListAccountCampaigns_PerCampaignReportFailure_MarksOnlyThatRowFailed`
   asserts one campaign's report failure marks only that row `FetchFailed`
   without affecting its siblings.
6. (`repo_code`) `docs/api-catalog.md`'s account-monitor row's 400-cause
   enumeration didn't mention `domain.ErrAccountNotManagedByConnection`
   (added in round 18), unlike `docs/knowledge/code/internal-service.md`'s
   own `classifyDiscoveryError` write-up. Extended the enumeration with the
   same wording already used there.

All six fixes preserve the existing "never fabricate a zero" rule already
documented on `model.AccountCampaignMetrics.FetchFailed` and exercised by
every sibling platform's own tests — this round found the remaining places
that rule wasn't yet wired through.
