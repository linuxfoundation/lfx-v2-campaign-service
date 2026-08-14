-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Widen the campaign slot key from (brief_id, platform) to
-- (brief_id, platform, variant), so one brief can hold a Search AND a Demand Gen
-- campaign on google-ads. See 000021 for why `variant` exists.
--
-- This index is LOAD-BEARING beyond uniqueness. ClaimCampaignDispatch elects a
-- single dispatch winner across replicas with
-- `INSERT ... ON CONFLICT (brief_id, platform, variant) WHERE status <> 'deleted'
-- DO NOTHING` -- that is what stops a retry creating a SECOND paid campaign
-- upstream. The repo's conflict target must name this exact column list AND
-- predicate or the statement fails at runtime with "there is no unique or
-- exclusion constraint matching the ON CONFLICT specification". The repo change
-- ships with this migration for that reason.
--
-- The partial predicate is carried over unchanged from 000013: excluding
-- soft-deleted rows is what lets a deleted campaign free its slot, while two LIVE
-- campaigns for the same triple are still rejected.
--
-- Widening a unique key can never reject a row the narrower key accepted, so this
-- is safe for existing data: every current row has variant='default' (000021's
-- DEFAULT), and (brief, platform, 'default') is unique exactly when
-- (brief, platform) was.
--
-- CONCURRENTLY, and alone in this file, for the reasons 000013 sets out: a
-- blocking build would stall in-flight claims during a rolling restart, and a
-- multi-statement file would be batched into a transaction, which CONCURRENTLY
-- cannot run inside. The old index is dropped in 000023, after this one is
-- proven present and valid.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_campaigns_brief_platform_variant_live
    ON campaigns (brief_id, platform, variant)
    WHERE status <> 'deleted';
