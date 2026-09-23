# 2026-09-17 monitor account endpoints — local review round 17

**Fix** — A seventeenth local pre-PR review round (general, repo_code, and
repo_learnings reviewers, all Claude Opus fallback since Pi remained
unavailable), still pinned to the branch's original base, found one Critical
issue from `general` and two Important/one Should-fix issues from
`repo_code`; `repo_learnings` was clean.

1. **Critical** — Round 16's fix was incomplete: it closed the
   credential-scope gap on Google/LinkedIn/Meta's monitor reads but not on
   Reddit's. `RedditDispatcher.resolveMonitorClient` still resolved via
   `d.creds.resolve` (the fallback-permitting resolver) and relied solely on
   an accountID-equality check against the resolved connection's own account
   to constrain access — but that check constrains *which* account is read,
   not *whose* credential is lent. A project with no Reddit connection of
   its own that supplied the shared LF system account's id (recoverable
   from its own past campaigns' `redditCreationAccountID`, if it ever
   dispatched through the fallback) satisfied the equality check and was
   served every campaign on that shared account, including ones dispatched
   by other projects through the same fallback. Fixed the same way as the
   other three platforms: `resolveMonitorClient`
   (`internal/dispatch/reddit.go`) now calls `d.creds.resolveOwned` instead
   of `d.creds.resolve`. Added
   `TestReddit_ListAccountCampaignMetrics_RefusesSystemFallback`, mirroring
   the three siblings added in round 16.

2. **Important** — `docs/knowledge/architecture/account-monitor-endpoints.md`
   and `docs/api-catalog.md` still described every dispatcher (or, post-fix,
   the endpoint as a whole) as resolving through the LF system-account
   fallback chain and returning 404 only when neither the project nor the
   system account has a connection — both false for Google/LinkedIn/Meta
   (and, after fix 1, Reddit too) since round 16. Rewrote the resolver
   bullet and added a new "Trust boundary" section to the architecture doc
   recording what round-16/17 changed, including the residual exposure the
   fix deliberately leaves open (dispatch/`ReadMetrics` still resolve with
   the fallback; Google Ads remains one shared customer across foundations
   even for a project with its own connection). Corrected the two false
   clauses in `docs/api-catalog.md`'s account-monitor row.

3. **Should fix** — `docs/knowledge/code/internal-dispatch.md`'s
   `credsSource` entry-point table still listed `resolveOwned`'s only caller
   as adoption. Extended the `resolveOwned` row's Callers cell to include
   the account-monitor resolvers.

Also dropped `resolveMonitorClient`'s now-dead `want != "" && got != ""`
emptiness checks (`internal/dispatch/reddit.go`): both `reddit.ValidateAccountID`
(called before any credential is resolved) and `resolveRedditClientWithCreds`'s
own empty-account-id refusal already guarantee both sides are non-empty by
the time this guard runs, and the stale architecture-doc text describing the
empty-id skip as reachable was corrected in the same edit as fix 2.
