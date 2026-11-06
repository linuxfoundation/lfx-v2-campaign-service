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
-- validation would fail with a less legible error. Either aborts the migration: the schema changes
-- roll back, but golang-migrate has already recorded version 56 as DIRTY, so no later boot retries
-- it (Up() refuses with ErrDirty, which startup treats as permanent). Recovery: resolve the rows
-- by hand, then `migrate force 55` and restart; the next boot runs 000056 again.
--
-- A LOCK WAIT NEVER FAILS IT, AND NEVER QUEUES TRAFFIC FOR LONG. ADD CONSTRAINT takes SHARE ROW
-- EXCLUSIVE on campaigns and campaign_briefs and the DROP takes ACCESS EXCLUSIVE on campaigns.
-- Queued behind one long transaction, either request would stall every later brief and campaign
-- write (and, for the DROP, read) that the N-1 pods serve during the rollout. So the DDL runs in a
-- retry loop: each attempt waits at most 2s for its locks (lock_timeout), and on 55P03 its
-- subtransaction rolls back, releasing every lock it took and leaving the queue, then sleeps 1s so
-- queued traffic drains. Failing the migration on a timeout instead would leave version 56 dirty
-- (see above), turning a busy table into a manual recovery, which is why the timeout is caught
-- rather than allowed to abort. After 30 attempts (about 90s) the last attempt waits without a
-- timeout, as every earlier migration here does, so a lock wait still ends in success.
--
-- campaign_jobs is deliberately NOT included: it has no project_id column, so a composite key
-- there needs a new column and a backfill, which this migration does not do. Its tenant scope
-- stays the JOIN through campaign_briefs (see JobRepo.GetJob).
DO $$
DECLARE
    mismatched BIGINT;
    attempt    INT := 0;
BEGIN
    SELECT count(*) INTO mismatched
      FROM campaigns c
      JOIN campaign_briefs b ON b.id = c.brief_id
     WHERE b.project_id <> c.project_id;
    IF mismatched > 0 THEN
        RAISE EXCEPTION '000056: % campaigns row(s) have a project_id that differs from their brief''s; resolve them by hand before migrating (this migration never rewrites data)', mismatched;
    END IF;

    LOOP
        attempt := attempt + 1;
        BEGIN
            -- is_local: scoped to this transaction, and reverted with the subtransaction.
            PERFORM set_config('lock_timeout', CASE WHEN attempt <= 30 THEN '2s' ELSE '0' END, true);

            -- Add the composite key before dropping the id-only one, so the table is never
            -- without a brief reference.
            ALTER TABLE campaigns
                ADD CONSTRAINT campaigns_brief_project_fkey
                FOREIGN KEY (brief_id, project_id)
                REFERENCES campaign_briefs (id, project_id);

            ALTER TABLE campaigns
                DROP CONSTRAINT IF EXISTS campaigns_brief_id_fkey;
            EXIT;
        EXCEPTION WHEN lock_not_available THEN
            RAISE NOTICE '000056: attempt % could not take its locks within 2s; retrying', attempt;
            PERFORM pg_sleep(1);
        END;
    END LOOP;
END $$;
