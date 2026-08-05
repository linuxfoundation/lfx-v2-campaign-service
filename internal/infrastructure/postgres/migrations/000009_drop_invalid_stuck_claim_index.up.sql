-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Clear an INVALID idx_campaigns_stuck_claims left by a failed CONCURRENTLY build in
-- 000008, and rebuild the index itself (not via retry of 000008).
--
-- Why this is needed at all: a failed `CREATE INDEX CONCURRENTLY` does NOT roll back —
-- it leaves the index in place marked INVALID, and Postgres refuses to use it. The
-- `IF NOT EXISTS` in 000008 then sees that name, skips the build, and reports success,
-- so the stuck-claim scan keeps full-scanning forever with no error anywhere. Recovering a
-- dirty migration with `force` marks version 8 applied WITHOUT running the down migration
-- or ever re-running the up migration, so 000008 will never retry the build.
--
-- Runs on EVERY deploy (it is a normal migration), but is a no-op unless an invalid
-- index is actually present.
--
-- Deliberately a plain DROP, not CONCURRENTLY: an INVALID index is not serving any
-- query, so dropping it blocks nothing that was working, and DROP INDEX CONCURRENTLY
-- cannot run inside the DO block this needs for the conditional.
--
-- This migration REBUILDS the index itself as a separate step. 000008 can never do it:
-- recovering the dirty schema requires `force`, which marks version 8 applied WITHOUT
-- running it, so golang-migrate never re-executes 000008 and its `IF NOT EXISTS` would
-- skip regardless. This migration runs on the NEXT deploy (its separate version number
-- ensures golang-migrate will execute it), so recovery requires operator force + a
-- subsequent deploy — "reissue the index build manually" or "wait for the next deploy to
-- apply this migration" are both correct.
--
-- The rebuild is a plain (non-CONCURRENT) CREATE, which is the opposite of 000008's
-- choice and is deliberate. CREATE INDEX CONCURRENTLY cannot run inside this DO block,
-- and it is only reachable at all on the recovery path — where the index is ALREADY
-- absent, so the scan is already degraded and a brief write lock is the cheaper of the
-- two costs. On the normal path (no invalid index) nothing here runs.
--
-- The table is schema-qualified. The index name is not: PostgreSQL's CREATE INDEX
-- grammar does not accept a schema-qualified index name — an index always lands in
-- whatever schema its parent table is in, so qualifying the table is correct and
-- sufficient. DROP INDEX is the one statement where qualifying the object itself is
-- both legal and meaningful, since DROP has no parent-table context to infer the
-- schema from.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        WHERE c.relname = 'idx_campaigns_stuck_claims'
          AND c.relnamespace = 'public'::regnamespace
          AND NOT i.indisvalid
    ) THEN
        EXECUTE 'DROP INDEX public.idx_campaigns_stuck_claims';
        RAISE NOTICE 'dropped INVALID idx_campaigns_stuck_claims from a failed CONCURRENTLY build; will rebuild below';
    END IF;
END
$$;

-- Rebuild the index with CONCURRENTLY (outside any transaction) to avoid blocking
-- campaign inserts/updates/deletes during the migration. This mirrors the approach
-- used in 000008, which chose CONCURRENTLY for the same reason: if an invalid
-- copy was dropped above, the index is now absent and will be created; if no invalid
-- copy existed, this becomes a no-op due to IF NOT EXISTS.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_campaigns_stuck_claims
    ON public.campaigns (created_at)
    WHERE status = 'pending';
