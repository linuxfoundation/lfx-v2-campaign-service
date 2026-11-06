-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000002's brief_id-only foreign key on campaigns.
--
-- Always applies and loses nothing: every row that satisfies the composite key satisfies the
-- id-only one. The UNIQUE (id, project_id) on campaign_briefs is 000007's and stays.
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_brief_id_fkey
    FOREIGN KEY (brief_id) REFERENCES campaign_briefs (id);

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_brief_project_fkey;
