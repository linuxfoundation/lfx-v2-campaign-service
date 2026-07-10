-- Reconcile the campaign_briefs / campaigns schema with the rest of the service.
-- This is a SEPARATE migration rather than an edit to 000002: golang-migrate
-- records applied versions and never re-runs them, so any change to an
-- already-applied migration would silently skip databases that already ran
-- 000002. A new version reconciles both fresh and previously-migrated DBs.
--
-- Two changes:
--
-- 1. project_id UUID -> TEXT. The connection tables (000001) store project_id as
--    TEXT because a project is addressed by UUID *or* slug (e.g. "cncf"), and the
--    API contract advertises the same. 000002 diverged by typing it UUID, which
--    would reject slug-addressed projects. Align briefs/campaigns with TEXT.
--
-- 2. Convert the (project_id, event_slug) uniqueness from a full table
--    constraint to a partial unique index that excludes archived rows, so
--    archiving a brief frees its (project_id, event_slug) slot for a new brief
--    (the full constraint reserved the slot permanently). Mirrors the connection
--    tables' partial-unique pattern; the index's leftmost column also covers
--    project_id-only lookups.

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_project_id_event_slug_key;

ALTER TABLE campaign_briefs
    ALTER COLUMN project_id TYPE TEXT;

ALTER TABLE campaigns
    ALTER COLUMN project_id TYPE TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS uq_campaign_briefs_project_event
    ON campaign_briefs (project_id, event_slug) WHERE status <> 'archived';
