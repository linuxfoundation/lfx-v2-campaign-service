-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Clear an INVALID idx_campaigns_stuck_claims left by a failed CONCURRENTLY build in
-- 000008, so the next attempt actually rebuilds it.
--
-- Why this is needed at all: a failed `CREATE INDEX CONCURRENTLY` does NOT roll back —
-- it leaves the index in place marked INVALID, and Postgres refuses to use it. The
-- `IF NOT EXISTS` in 000008 then sees that name, skips the build, and reports success,
-- so the stuck-claim scan keeps full-scanning forever with no error anywhere. Nothing
-- else clears it: recovering a dirty migration with `force` marks the version applied
-- WITHOUT running the down migration.
--
-- Runs on EVERY deploy (it is a normal migration), but is a no-op unless an invalid
-- index is actually present.
--
-- Deliberately a plain DROP, not CONCURRENTLY: an INVALID index is not serving any
-- query, so dropping it blocks nothing that was working, and DROP INDEX CONCURRENTLY
-- cannot run inside the DO block this needs for the conditional.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        WHERE c.relname = 'idx_campaigns_stuck_claims'
          AND NOT i.indisvalid
    ) THEN
        EXECUTE 'DROP INDEX idx_campaigns_stuck_claims';
        RAISE NOTICE 'dropped INVALID idx_campaigns_stuck_claims; 000008 will rebuild it on the next deploy';
    END IF;
END
$$;
