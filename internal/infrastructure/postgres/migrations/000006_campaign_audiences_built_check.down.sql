-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

ALTER TABLE campaign_audiences
    DROP CONSTRAINT IF EXISTS campaign_audiences_built_needs_master_list;
