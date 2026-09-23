# 2026-09-17 monitor account endpoints — local review round 15

**Fix** — A fifteenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
four Important issues from `general`; `repo_code` and `repo_learnings` were
both clean.

1. Meta's campaign-list filter (`internal/platform/meta/monitor.go`, ported
   verbatim from `getMetaAnalytics`'s `impressions>0 || status==='ACTIVE'`)
   silently dropped a PAUSED campaign whose insights row failed to parse: a
   `FetchFailed` row leaves `Impressions` at 0 and the campaign is not
   `ACTIVE`, so it never passed the filter and the fetch failure — the exact
   thing round 14 made `FetchFailed` exist to surface — was invisible instead
   of reported. Fixed by admitting a row into the monitor view when
   `FetchFailed` is true, regardless of the ported filter. Added
   `TestListAccountCampaigns_PausedCampaignFetchFailed_IsNotDropped` (a PAUSED
   campaign, malformed insights row); the existing
   `TestListAccountCampaigns_MalformedInsightsRow_MarksFetchFailed` used an
   `ACTIVE` campaign, which already passed the old filter and so could not
   have caught this.
2. `internal/service/rules/monitor_google.go`'s `FetchFailed` branch is
   currently unreachable in production — Google's GAQL read has no
   per-campaign partial-failure mode, so nothing in
   `internal/platform/googleads` ever sets `FetchFailed=true` on a real row —
   while `fetchFailedRow`'s doc comment in `monitor_shared.go` and
   `TestEvaluateGoogleMonitor_SkipsFetchFailedRows`'s comment both implied the
   branch was live and pinned today. Reworded all three comments (the shared
   helper, the branch itself, and the test) to say plainly that the Google
   branch is defensive/forward-looking, not reachable yet, rather than letting
   the prose overstate what the code currently does.
3. `internal/apivalidation/monitor_account_id_drift_test.go`'s
   `monitorDaysDecoderChecker` had a comment that was wrong on two counts: it
   claimed the function "takes the account_id to use rather than guessing"
   when it takes no such parameter and hardcodes a value per `path` (the
   guessing the sentence disclaimed), and it claimed a digits-only id fails
   Reddit's pattern when Reddit's design `Pattern` (`^[A-Za-z0-9_]+$`) already
   accepts digits-only, making that `case` branch not load-bearing. Reworded
   the comment to describe the switch accurately rather than asserting an
   untrue premise.
4. `internal/dispatch/googleads.go`'s `ListAccountCampaignMetrics` (and its
   LinkedIn/Meta siblings) validate `accountID` for shape only, never for
   ownership: when the resolved credential falls back to the shared LF system
   connection, a project with no connection of its own can read another
   project's spend/budget/campaign data for any account id that credential
   can reach. This predates the branch (the same gap the already-shipped
   `ListAccounts` discovery endpoint has, per `64141f23`) and is not fixed in
   this round — it is a trust-boundary decision beyond a threshold/wording
   fix. Documented the gap explicitly on all three dispatcher methods
   (Google/LinkedIn/Meta) so it is visible rather than silently inherited;
   Reddit's dispatcher is unaffected because it already validates `accountID`
   against the resolved connection's own single account rather than building
   an account-agnostic client.

Fixes 1-3 are code/comment corrections; fix 4 is documentation only, pending a
follow-up decision on the pre-existing credential-scope gap.
