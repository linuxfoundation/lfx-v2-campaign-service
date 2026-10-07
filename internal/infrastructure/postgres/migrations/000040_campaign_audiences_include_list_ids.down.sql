-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops include_list_ids. platform_master_list_id already holds each row's FIRST include
-- id, so every built audience stays dispatchable -- but an audience attached to several lists
-- sends to the first of them only after the rollback. Re-attach such audiences if the full set
-- matters.

ALTER TABLE campaign_audiences DROP COLUMN IF EXISTS include_list_ids;
