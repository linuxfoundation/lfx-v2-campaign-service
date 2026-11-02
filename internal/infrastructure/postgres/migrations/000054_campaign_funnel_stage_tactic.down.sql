-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop campaigns.funnel_stage and campaigns.tactic.
--
-- Always applies, and LOSES the labels: every campaign created with a stage or tactic reverts to
-- an unlabelled row. The upstream campaigns keep their names (a Microsoft campaign created with a
-- stage keeps the stage token in its name), so after a rollback the name is the only record of the
-- stage. Before rolling back past this migration, turn off the self-serve multi-campaign platform
-- list so no new labelled campaign is created, and export the labels if they must be kept (see the
-- rollback runbook in docs/knowledge/kubernetes/deployment.md).
--
-- The constraints go with their columns; they are dropped first only so the statement order reads
-- as the reverse of the up migration.

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_tactic_valid,
    DROP CONSTRAINT IF EXISTS campaigns_funnel_stage_valid;

ALTER TABLE campaigns
    DROP COLUMN IF EXISTS tactic,
    DROP COLUMN IF EXISTS funnel_stage;
