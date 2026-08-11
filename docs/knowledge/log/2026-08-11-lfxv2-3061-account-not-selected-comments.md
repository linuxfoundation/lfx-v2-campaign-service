# 2026-08-11 — LFXV2-3061: correct three stale claims about ErrAccountNotSelected

**Docs** — a contract change made three source comments false without touching a line of
their behaviour.

Making `MetaAdsConnectionConfig` credentials-only added a second credentials-first provider
and, separately, a second producer of `domain.ErrAccountNotSelected` on a path that had never
had one. Three comments still described the single-provider, all-synchronous world:

1. `internal/domain/errors.go` said the state "became a SUPPORTED state when
   `GoogleAdsConnectionConfig` dropped `Required("account_id")`", implying Google Ads is the
   only such provider. `MetaAdsConnectionConfig` now requires only `page_id`.
2. The same block said the sentinel "reaches exactly two handlers … both of which answer
   409". The two synchronous handlers are unchanged, but Meta's `requireMetaAccountID`
   (`internal/dispatch/meta.go`) tags it from `Dispatch` — queued work, where
   `dispatchPlatform` collapses every dispatcher error into one job-result string and the
   reason token reaches an operator through the LOG only. **409 is no longer this sentinel's
   universal fate**, which is exactly the assumption a future error-mapping change would make.
3. `internal/service/brief.go` said "Only Google Ads has one
   (design/connection.go, list-google-ads-accounts)". `design/connection.go` now declares
   `list-meta-ads-accounts` too.

Found by Copilot on PR #116 as a suppressed comment.

## What was measured, not assumed

The rewritten sentences name a specific set of producers, so that set was enumerated rather
than inferred:

    grep -rn "ErrAccountNotSelected" internal/ | grep -v _test

- **Synchronous producers** — `validateGoogleAdsConnection`, `validateMicrosoftConnection`,
  `validateTwitterConnection` and `RedditDispatcher.resolveRedditClient`, each reached from
  that dispatcher's `ToggleStatus`/`ReadMetrics`.
- **Asynchronous producer** — `requireMetaAccountID`, whose only caller is
  `MetaDispatcher.Dispatch`.
- **Consumers** — `ReadCampaignMetrics` and `ToggleCampaignStatus` in
  `internal/service/brief.go` (both 409), plus `unusableConnectionReason`
  (`internal/service/connection.go`), which is a log-token classifier and not a handler.

Meta therefore has no synchronous producer at all: its toggle and metrics reads address the
campaign node by id and need no account id.

## The rule

A comment that enumerates ("exactly two", "only Google Ads") is a claim with a shelf life.
When a change adds a member to a set some comment counts, the comment is part of the change's
blast radius even though nothing references it. The counts here were true when written; the
fix is not to soften them into vagueness but to re-derive them — and the enumerating clauses
that were PRESERVED in each rewritten sentence were re-checked too, not carried over on trust.
