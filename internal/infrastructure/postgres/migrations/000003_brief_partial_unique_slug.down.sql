-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Revert to the 000002 schema: full uniqueness constraint and UUID project_id.
-- The UUID casts assume every stored project_id is a valid UUID; a slug-valued
-- row would fail the cast, which is the intended signal that the down migration
-- is unsafe once slug-addressed projects exist.
DROP INDEX IF EXISTS uq_campaign_briefs_project_event;

ALTER TABLE campaigns
    ALTER COLUMN project_id TYPE UUID USING project_id::uuid;

ALTER TABLE campaign_briefs
    ALTER COLUMN project_id TYPE UUID USING project_id::uuid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_project_id_event_slug_key
    UNIQUE (project_id, event_slug);
