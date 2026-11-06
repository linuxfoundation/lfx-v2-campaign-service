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
-- ADD ONLY, NO DROP. 000002's id-only campaigns_brief_id_fkey stays. It is redundant (the
-- composite key implies it), and dropping it would need ACCESS EXCLUSIVE on campaigns, which
-- blocks reads, and an ADD -> DROP lock upgrade inside one transaction, which can deadlock against
-- an ordinary read-then-update. ADD CONSTRAINT alone takes SHARE ROW EXCLUSIVE on campaigns and
-- campaign_briefs: writes to either wait for it, reads never do. A later migration may drop the
-- redundant key once that cost is worth paying.
--
-- BOUNDED, RETRIED LOCK WAITS. Queued behind one long transaction, the ADD's lock request would
-- hold every later campaign and brief write behind it. So it runs in a retry loop: each attempt
-- waits at most 2s (lock_timeout), and on 55P03 or 40P01 (a deadlock with a transaction that
-- updates a brief and then writes a campaign, which PostgreSQL resolves by aborting the attempt)
-- its subtransaction rolls back, releasing the locks it took and its place in the queue, and
-- sleeps 1s so queued writes drain. A caught failure is not a migration failure, so it does not
-- dirty the version.
--
-- The loop is bounded at 15 attempts (about 45s of lock waiting), well inside the chart's ~90s
-- startupProbe budget: migrations run at boot, and a pod killed mid-migration would leave version
-- 56 dirty anyway. lock_timeout bounds lock ACQUISITION only. The attempt that gets its locks
-- holds them while it validates every existing campaigns row, so writes also wait for that scan,
-- and neither it nor the precheck above has a time limit of its own. Both scale with the table,
-- which is small. If every attempt fails, it raises, which leaves version 56 dirty like the precheck
-- does: wait out or end the blocking transaction, then `migrate force 55` and restart.
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
            -- is_local: scoped to this transaction.
            PERFORM set_config('lock_timeout', '2s', true);
            ALTER TABLE campaigns
                ADD CONSTRAINT campaigns_brief_project_fkey
                FOREIGN KEY (brief_id, project_id)
                REFERENCES campaign_briefs (id, project_id);
            EXIT;
        EXCEPTION WHEN lock_not_available OR deadlock_detected THEN
            IF attempt >= 15 THEN
                RAISE EXCEPTION '000056: could not take the campaigns / campaign_briefs locks in % attempts (last: %); end the blocking transaction, then migrate force 55 and restart', attempt, SQLERRM;
            END IF;
            RAISE NOTICE '000056: attempt % failed (%); retrying', attempt, SQLERRM;
            PERFORM pg_sleep(1);
        END;
    END LOOP;
END $$;
