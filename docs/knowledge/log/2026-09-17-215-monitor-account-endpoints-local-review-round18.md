# 2026-09-17 monitor account endpoints — local review round 18

**Fix** — An eighteenth local pre-PR review round (general and repo_code
Claude Opus fallback, Pi still unavailable), pinned to the branch's original
base, found no Critical or Important issues from `repo_code` and three
Important issues from `general` — one of which `repo_code` independently
flagged as a Should-fix. `repo_learnings` was clean.

1. **Important** (general Important #1, repo_code Should-fix #1) —
   `RedditDispatcher.ReadAccountTotals` called `resolveMonitorClient`
   directly, without first validating `accountID`/`days` the way its sibling
   `ListAccountCampaignMetrics` does — even though `resolveMonitorClient`'s
   own doc comment (added in round 17) claimed *"reddit.ValidateAccountID
   (called by every caller of this method before it resolves any
   credential)"*, which was false for this caller. No live behavior broke
   (the only production entry point, `monitorAccount`, always calls
   `ListAccountCampaignMetrics` first), but a future non-`monitorAccount`
   caller with an empty or malformed `accountID` would have resolved a
   credential unnecessarily and then been told its stored connection was
   broken. Added the same two guards (`reddit.ValidateAccountID`,
   `validateMonitorDays`) to the top of `ReadAccountTotals`, making the doc
   comment literally true again, plus two regression tests
   (`TestReddit_ReadAccountTotals_RejectsMalformedAccountID`,
   `TestReddit_ReadAccountTotals_RejectsInvalidDays`).

2. **Important** — a well-formed account id that simply isn't the one the
   project's own Reddit connection resolves to was classified as
   `domain.ErrConnectionNotUsable`, whose message tells the operator to
   check that the stored credential is active and valid — but the stored
   connection is fine; the *request* named the wrong account. Added a new
   sentinel `domain.ErrAccountNotManagedByConnection`, a
   `classifyDiscoveryError` arm (checked before `ErrConnectionNotUsable`)
   returning 400 with a message naming the real remedy, and switched
   `resolveMonitorClient`'s mismatch branch to wrap it. Added
   `TestReddit_ListAccountCampaignMetrics_RejectsMismatchedAccount` to pin
   the new classification and confirm the old sentinel is no longer
   attached.

3. **Important** — `internal/platform/googleads/monitor.go`'s
   `ListAccountCampaigns` derives its `[start, end]` GAQL window from `days`
   but its doc comment, unlike the Meta/LinkedIn siblings that took the same
   signature change, said nothing about trusting `days` from the service
   layer rather than validating it locally. Copied the Meta sibling's
   days-trust paragraph onto the Google Ads doc comment so all three
   platform clients state the same contract.

Also documented the new sentinel in
`docs/knowledge/code/internal-service.md`'s `classifyDiscoveryError`
mapping list, alongside `ErrAccountIDMalformed` and `ErrMonitorDaysInvalid`.
