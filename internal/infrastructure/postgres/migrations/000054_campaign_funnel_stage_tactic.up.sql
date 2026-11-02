-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add campaigns.funnel_stage and campaigns.tactic: the label a campaign was CREATED with, so one
-- event can run several campaigns on the same platform — top-, mid- and lower-funnel, by
-- different tactics — as first-class, labelled campaigns rather than unexplained duplicates.
--
-- funnel_stage is one of 'top', 'mid', 'lower'. tactic is one of a controlled list
-- ('prospecting', 'retargeting', 'lookalike', 'keyword', 'competitor', 'content-promotion'): a
-- free-text label would reach campaign names, and the data pipeline's fallback parsers match
-- words such as "retarget" or "tofu" anywhere in a name, so v1 keeps the vocabulary closed.
-- model.FunnelStage / model.Tactic mirror both lists; TestLiveCampaignLabelChecksMatchTheModel
-- pins them.
--
-- SET AT CLAIM, IMMUTABLE AFTER: the dispatch claim INSERT writes them and no UPDATE names them
-- (the upsert's conflict arm and replaceCampaignQuery leave both columns alone), so a retry that
-- carries a different stage can never relabel — or, through the name, rename — a campaign that
-- already exists. Upstream names are composed from the CLAIMED ROW's values, never the request.
--
-- NOT in the slot's unique key (brief_id, platform, variant, slot_version): two campaigns on one
-- platform are told apart by slot_version, and the stage is a label on each, not part of its
-- identity. Putting it in the key would let a retry carrying a different stage claim a second
-- slot alongside the first — the accidental duplicate the claim exists to prevent.
--
-- EXPAND-only, nullable, no default, no backfill: NULL means a LEGACY campaign — created before
-- this column, created without a stage, or adopted — and its upstream name stays byte-identical
-- to what it always was. The N-1 binary neither reads nor writes either column (its INSERTs and
-- UPDATEs name their columns), so a rolling deploy is safe in both directions.

ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS funnel_stage TEXT,
    ADD COLUMN IF NOT EXISTS tactic TEXT;

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_funnel_stage_valid;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_funnel_stage_valid
    CHECK (funnel_stage IS NULL OR funnel_stage IN ('top', 'mid', 'lower'));

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_tactic_valid;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_tactic_valid
    CHECK (tactic IS NULL OR tactic IN (
        'prospecting', 'retargeting', 'lookalike', 'keyword', 'competitor', 'content-promotion'
    ));
