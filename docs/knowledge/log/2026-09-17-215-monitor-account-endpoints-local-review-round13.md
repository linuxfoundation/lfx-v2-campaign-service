# 2026-09-17 monitor account endpoints — local review round 13

**Fix** — A thirteenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers), still pinned to the branch's original base, found
two Important issues from `general` and one Should-fix from `repo_code`;
`repo_learnings` was clean.

1. `domain.ErrMonitorDaysInvalid` (added last round) had no test at any
   level, unlike its sibling `domain.ErrAccountIDMalformed`, which got a
   dispatcher test, a classifier table row, and the cross-layer drift test.
   Fixed by adding `TestReddit_ListAccountCampaignMetrics_RejectsInvalidDays`
   (mirroring the existing malformed-account-id dispatcher test) and a
   `"invalid days maps to 400"` row to
   `TestMonitorAccount_ClassifiesDiscoveryError`'s table in
   `internal/service/connection_monitor_test.go`.
2. The `days` 7..90 bound existed as five independent copies — the design
   layer's four `Minimum(7)/Maximum(90)` pairs, `internal/service`'s
   `validateMonitorDays`, `internal/dispatch`'s `validateMonitorDays`, and
   `ErrMonitorDaysInvalid`'s own message text — with no guard against them
   drifting apart, unlike `account_id`'s shape check, which the existing
   `monitor_account_id_drift_test.go` already pins across layers. Fixed by
   hoisting `domain.MonitorDaysMin`/`MonitorDaysMax` constants, having both
   runtime `validateMonitorDays` copies and `ErrMonitorDaysInvalid`'s message
   read them, collapsing design's four literal pairs to one local
   `monitorDaysMin`/`monitorDaysMax` const pair (design deliberately does not
   import `internal/domain` — see its package doc), and adding
   `TestMonitorDaysBound_MatchesDomainConstants` to
   `monitor_account_id_drift_test.go`, driving the real generated decoder
   from the runtime constants' boundary for all four platforms. `goa gen`
   regeneration produced a byte-identical `gen/**`/`kodata/**` diff (the
   named constants resolve to the same literal values), so nothing there
   needed re-copying.
3. `classifyDiscoveryError`'s new `ErrMonitorDaysInvalid` arm was missing
   from `docs/knowledge/code/internal-service.md`'s enumeration of that
   switch, unlike its sibling `ErrAccountIDMalformed`, which was documented
   in the same round it was added. Fixed by adding the matching bullet
   between the `ErrAccountIDMalformed` bullet and the `Anything else → 503`
   line.

All three fixes extend the account_id precedent from prior rounds (test
coverage, cross-layer drift guard, concept-doc currency) to `days`, rather
than introducing a new pattern.
