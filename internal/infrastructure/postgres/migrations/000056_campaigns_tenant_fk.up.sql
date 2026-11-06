-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Composite tenant foreign key on campaigns (#534, the #165 audit's optional hardening): a
-- campaigns row's project_id MUST match the project_id of its parent brief. 000002 references
-- campaign_briefs(id) alone, so brief_id is valid but the copied project_id is unchecked -- the
-- gap 000007, 000028 and 000033 closed for campaign_audiences, creative_assets and
-- wizard_sessions. Every writer already pairs the two: the dispatch claim and upsert stamp
-- brief.ProjectID / brief.ID from the loaded brief, and the adopt INSERT runs under
-- lockAdoptBriefQuery, which is keyed on (id, project_id). The FK makes that the datastore's
-- rule for every writer, and lets lockBriefRowForReplaceQuery key its lock on both columns
-- without the predicate ever silently missing.
--
-- The UNIQUE (id, project_id) a composite FK references already exists
-- (campaign_briefs_id_project_id_key, 000007).
--
-- FAILS LOUDLY, NEVER REPAIRS. A row whose project_id disagrees with its brief's is a tenant
-- boundary question a person must answer (which project does that paid campaign belong to?), not
-- one a migration may guess at. The explicit check names the count before ADD CONSTRAINT's own
-- validation would fail with a less legible error; both abort the migration and leave the schema
-- at 000055.
--
-- campaign_jobs is deliberately NOT included: it has no project_id column, so a composite key
-- there needs a new column and a backfill, which this migration does not do. Its tenant scope
-- stays the JOIN through campaign_briefs (see JobRepo.GetJob).
-- BOUNDED LOCK WAIT. ADD CONSTRAINT takes SHARE ROW EXCLUSIVE on campaigns and campaign_briefs and
-- the DROP takes ACCESS EXCLUSIVE on campaigns, all held to COMMIT. Queued behind one long
-- transaction, either would stall every brief and campaign read the N-1 pods serve during the
-- rollout. SET LOCAL scopes the timeout to this file's implicit transaction: a timeout aborts it
-- cleanly at 000055, and the next boot retries.
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    mismatched BIGINT;
BEGIN
    SELECT count(*) INTO mismatched
      FROM campaigns c
      JOIN campaign_briefs b ON b.id = c.brief_id
     WHERE b.project_id <> c.project_id;
    IF mismatched > 0 THEN
        RAISE EXCEPTION '000056: % campaigns row(s) have a project_id that differs from their brief''s; resolve them by hand before migrating (this migration never rewrites data)', mismatched;
    END IF;
END $$;

-- Add the composite key before dropping the id-only one, so the table is never without a brief
-- reference (the migration runs in one transaction either way).
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_brief_project_fkey
    FOREIGN KEY (brief_id, project_id)
    REFERENCES campaign_briefs (id, project_id);

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_brief_id_fkey;
