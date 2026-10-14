-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- One live row per Reddit Ads campaign PER AD ACCOUNT (LFXV2-2665).
--
-- ACCOUNT-SCOPED key, because Reddit does not document its campaign ids as globally unique: every
-- Ads API v3 campaign path is account-scoped (/ad_accounts/{account_id}/campaigns/{campaign_id},
-- https://ads-api.reddit.com/docs/v3/), and no read addresses a campaign by id alone. Keying on the
-- account is exact either way: if the ids are global, one id always carries one account and the
-- key still refuses every duplicate; if they are per account, it cannot false-reject. The account
-- is result.accountId, exactly as redditCreationAccountID reads it (Reddit has no URL fallback).
--
-- LEGACY ROWS. The key carries the account EXTRACTED from the row's result provenance JSON
-- (the field each dispatcher's *CreationAccountID reader uses). A row that records no account
-- yields NULL, and NULLs are distinct in a unique index, so such a row can never block a
-- legitimate binding — and AdoptCampaign separately refuses (409 AlreadyBound) to adopt an id that
-- a live account-less row already holds, so an unknown-account row cannot slip a second binding
-- through adoption either. Every current write path (create and adopt) stamps the account.
--
-- A SEPARATE index per provider, never a wider 000020/000042 predicate — see 000020 and the
-- LFXV2-3056 log entry: whether a global key is right is a per-provider question.
--
-- AdoptCampaign and UpsertCampaign classify its 23505 as ErrPlatformCampaignAlreadyBound
-- (isPlatformCampaignBindingViolation): adoption answers the "already bound to another brief"
-- 409, and a dispatch that resolved to an already-bound campaign is reported as that named
-- conflict with its claim retained.
--
-- status <> 'deleted' and platform_campaign_id IS NOT NULL for 000020's reasons.
--
-- CONCURRENTLY, one statement per file, no transaction, and NO `IF NOT EXISTS` — 000020's
-- reasons: a retry over an INVALID leftover must fail with 42P07, not record this version clean
-- over an index that enforces nothing. DEPLOY RISK: if live rows already bind one upstream
-- campaign twice, the build fails and the deploy stops; resolve the duplicate binding (soft-delete
-- one row), drop the INVALID index, and re-run.
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_reddit_campaign_live
    ON campaigns (platform, platform_campaign_id, (NULLIF(result ->> 'accountId', '')))
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'reddit-ads';
