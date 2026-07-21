-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore the brief_id-only foreign key.
ALTER TABLE campaign_audiences
    DROP CONSTRAINT IF EXISTS campaign_audiences_brief_project_fkey;

ALTER TABLE campaign_audiences
    ADD CONSTRAINT campaign_audiences_brief_id_fkey
    FOREIGN KEY (brief_id) REFERENCES campaign_briefs (id);

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_id_project_id_key;
