# 2026-09-17 monitor account endpoints — local review round 4

**Fix** — A fourth local pre-PR review round (general, repo_code, and
repo_learnings reviewers) on the account-monitor-endpoints branch converged on
one shared finding and surfaced two more:

1. `repo_learnings` and `general` both flagged that round 3's fix for
   LinkedIn/Meta's dispatcher-level shape guard (see round 3, finding #5) left
   two statements stale: `internal/domain/errors.go`'s
   `ErrAccountIDMalformed` doc comment and `docs/api-catalog.md`'s
   status-mapping sentence both still claimed the sentinel applied to
   "Google Ads/Reddit only," which was true before that fix and false after
   it — `docs/reviews/knowledge-base/api-contract-and-docs-currency.md`'s
   `docs-must-not-advertise-what-the-code-rejects` pattern. Fixed by
   rewriting both to describe all four dispatchers validating shape before
   resolving a credential, with LinkedIn/Meta additionally refused earlier
   still at the Goa layer for an ordinary HTTP caller. `docs/api-catalog.md`'s
   Pattern/MinLength paragraph also gained a clarifying sentence that
   LinkedIn's and Meta's dispatchers now carry the same shape check too, as
   defense-in-depth for a non-HTTP caller.
2. `general` flagged that `internal/dispatch/reddit_test.go`'s comment on
   `TestReddit_ListAccountCampaignMetrics_RejectsMalformedAccountID` was
   stale as of the round-3 commit: it described the empty id "reaching
   ListAccountCampaigns' own accountIDRe check untouched," but
   `reddit.ValidateAccountID` (added in round 3) now rejects it before
   `resolveMonitorClient` or `ListAccountCampaigns` ever run — so the test
   exercises the dispatch-level guard, not the inner
   `reddit.ErrInvalidCampaignID` branch the comment described, and that inner
   branch is now effectively unreachable and uncovered. Rewrote the comment
   to describe what the test actually exercises today; left the inner branch
   itself uncovered by a dedicated test, since it can only fire if the
   platform client's own regex ever diverges from `ValidateAccountID`'s
   (the same regex, by construction) — not a scenario a fake or stub can
   represent without asserting a divergence that cannot happen.
3. `general` flagged (finding #1) that `monitorAccount`'s totals-failure log
   (`internal/service/connection_monitor.go`) always logged
   `unusableConnectionReason(terr)` regardless of what `terr` actually was —
   correct for `domain.ErrConnectionNotUsable` (whose detection path decodes
   a decrypted credential blob and must never leak into a log), but for any
   other failure (a plain upstream/network error) it silently discarded the
   real cause behind a fixed-vocabulary token that doesn't apply. Fixed by
   branching on `errors.Is(terr, domain.ErrConnectionNotUsable)`: that arm
   keeps logging the fixed-vocabulary reason exactly as before, everything
   else now logs `terr` directly.
4. `general` also flagged (finding #4) that `ReadAccountTotals(...,
   len(rows))` passes the post-rule-engine row count with no comment
   explaining why that's safe — it's an implicit assumption that no
   `AccountTotalsReader` implementation filters rows the way the rule
   engines above it do. Added a comment stating the assumption explicitly at
   the call site.

No new correctness bugs found this round; all three findings are
documentation/comment-currency and log-fidelity fixes.
