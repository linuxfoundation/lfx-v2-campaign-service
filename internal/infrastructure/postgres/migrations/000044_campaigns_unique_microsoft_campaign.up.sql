-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- One live row per Microsoft Advertising campaign PER AD ACCOUNT (LFXV2-2665).
--
-- ACCOUNT-SCOPED key: Microsoft campaign ids are minted per ad account and this service supports
-- separate per-project Microsoft connections (docs/knowledge/log/2026-08-10-lfxv2-3056-index-scope-google-ads.md;
-- every Campaign Management call is scoped by CustomerAccountId,
-- https://learn.microsoft.com/en-us/advertising/campaign-management-service/getcampaignsbyids).
-- A global key would false-reject account B minting an id account A already used, so the key adds
-- the account the row was created under: result.accountId, or — for rows written before that
-- field existed — the aid= query parameter of result.microsoftAdsUrl, exactly as
-- microsoftCreationAccountID reads it.
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
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_microsoft_campaign_live
    ON campaigns (platform, platform_campaign_id,
        (COALESCE(NULLIF(result ->> 'accountId', ''), NULLIF(substring(result ->> 'microsoftAdsUrl' from '[?&]aid=([^&#]+)'), ''))))
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL AND platform = 'microsoft-ads';
