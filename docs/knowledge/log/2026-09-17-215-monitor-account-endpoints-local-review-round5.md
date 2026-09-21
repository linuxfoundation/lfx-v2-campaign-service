# 2026-09-17 monitor account endpoints — local review round 5

**Fix** — A fifth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
one Important issue and three should-fix issues:

1. `general` flagged (Important #1) that `internal/dispatch` reused
   `reddit.ErrInvalidCampaignID` for a caller-supplied *account* id shape
   failure in `ValidateAccountID`, `ListAccountCampaigns`, and
   `FetchAccountTotals` — the same sentinel campaign-id shape failures also
   wrap, so `errors.Is` callers and logs cannot tell "bad account id" from
   "bad campaign id" apart. Fixed by adding a distinct
   `reddit.ErrInvalidAccountID` sentinel in `internal/platform/reddit/metrics.go`
   and switching all three sites plus `internal/dispatch/reddit.go`'s
   defense-in-depth branch and `internal/dispatch/reddit_test.go`'s assertion
   to it.
2. `general` flagged (Important #2) that the `FetchFailed`-row construction
   was quadruplicated verbatim across all four `EvaluateXMonitor` functions
   (google/linkedin/meta/reddit) in `internal/service/rules/monitor_*.go`.
   Extracted a shared `fetchFailedRow` helper into `monitor_google.go` (the
   file the other three's comments already pointed to as canonical) and
   replaced each duplicate with a three-line call to it. The four files'
   pacing/threshold logic remains deliberately unshared per the
   differential-diff-faithfulness rationale in `monitor_google.go`'s
   package comment and follow-up ticket #7
   (`linuxfoundation/lfx-self-serve#2519`); only this one shared,
   contract-level invariant (skip evaluation for a failed-fetch row) is now
   centralized.
3. `repo_learnings` flagged (should-fix #1, citing
   `docs/reviews/knowledge-base/api-contract-and-docs-currency.md`'s
   `docs-must-not-advertise-what-the-code-rejects` pattern) that round 4's
   fix for `domain.ErrAccountIDMalformed`'s documentation missed a third
   site: `internal/service/connection.go`'s `classifyDiscoveryError` switch
   arm comment still described the sentinel as applying only to a "caller-
   supplied account id, not a stored connection," without accounting for
   LinkedIn/Meta's earlier Goa-layer rejection added in round 3. Rewrote the
   comment to describe all four dispatchers reaching this arm and note that
   LinkedIn/Meta additionally carry a Goa `Pattern`, so their non-HTTP
   callers are the ones this classification actually serves.
4. `repo_learnings` flagged (should-fix #2) that
   `internal/platform/meta/monitor.go`'s `ListAccountCampaigns` doc comment
   claimed `MinLength/Pattern/MaxLength` all apply to `accountID` at the
   design layer, but the design doc
   (`docs/knowledge/architecture/account-monitor-endpoints.md`) documents
   Pattern/MaxLength only — no MinLength, since the regex itself already
   rejects an empty string. Rewrote the comment to match.
5. `repo_code` flagged (should-fix #1) that `docs/knowledge/kubernetes/httproute.md`
   and `docs/knowledge/kubernetes/ruleset.md` went stale when an earlier
   commit in this branch range added `/account-monitor` to the Helm
   manifests for google-ads/meta-ads/linkedin-ads and reddit-ads: both docs
   still described the pre-`/account-monitor` branch counts and per-provider
   path sets (httproute.md claiming "Reddit carries neither" and only three
   alternation branches). Rewrote both to describe the current four branches
   — google-ads/meta-ads/linkedin-ads with `/accounts` + `/account-monitor`,
   microsoft-ads/twitter-ads with `/accounts` only, `connection-hubspot`
   with `/emails`+`/campaigns`, and `connection-reddit-ads` with
   `/account-monitor` but no `/accounts` (it implements
   `AccountMetricsReader` but not `AccountLister`) — while preserving the
   exact "the providers with neither (...)" phrasing
   `TestAccountListerProseMatchesTheInterface`
   (`internal/dispatch/accountlister_prose_parity_test.go`) binds to for the
   `AccountLister` roster.

No new correctness bugs found this round; all four findings are
documentation/comment-currency, sentinel-disambiguation, and duplication
fixes.
