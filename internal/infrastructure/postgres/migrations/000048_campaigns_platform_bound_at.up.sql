-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Records WHEN a campaign row took its upstream binding (platform_campaign_id), which created_at
-- does not (lfx-self-serve-ops#897).
--
-- The account monitor's deleted-campaign scope (listProjectPlatformDeletedCampaignIDsQuery) hands
-- an upstream campaign to its LATEST owner once every project has deleted its row. created_at
-- cannot order that: a dispatch's created_at is when ClaimCampaignDispatch inserted its 'pending'
-- placeholder, BEFORE the platform call, and upsertCampaignQuery fills the upstream id later
-- without touching it. A dispatch that binds an EXISTING upstream campaign (Google's
-- adoptExisting, Meta's same-name reuse) can claim, wait out another project's adopt-then-delete
-- of that campaign, and then bind it, becoming the current owner with the OLDER created_at.
-- Ordered by created_at, the former owner would get the campaign's budget and spend back.
--
-- EXPAND ONLY, under README.md's rule: nullable, no default. An N-1 binary's writes name their
-- columns explicitly and leave it NULL, which readers treat as "bound when the row was created"
-- and fall back to created_at — exact for adoptions and for rows that predate this column, whose
-- only evidence is created_at anyway. A DEFAULT now() would be WRONG rather than merely
-- imprecise: it would stamp a dispatch claim at insertion, which is the very ordering this column
-- exists to replace. No backfill for the same reason: COALESCE(platform_bound_at, created_at)
-- reads a NULL exactly as a backfill from created_at would write it.
--
-- NO INDEX: it is never a search predicate. Its one reader compares it between rows the NOT
-- EXISTS has already matched on (platform, platform_campaign_id).
ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS platform_bound_at TIMESTAMPTZ;
