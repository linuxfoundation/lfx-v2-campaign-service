-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Clear an INVALID idx_campaigns_stuck_claims left by a failed CONCURRENTLY build in
-- 000008. The rebuild itself lives in 000015, a separate migration file: the pgx/v5
-- golang-migrate driver executes each file with a bare ExecContext and does NOT wrap
-- it in a transaction, but it also does not split a file into per-statement
-- executions — a file with more than one statement is sent to Postgres as one
-- implicit transaction block, and CREATE INDEX CONCURRENTLY cannot run inside one
-- (see 000008's own "Do NOT add other statements to this file" warning). Splitting
-- the drop and the rebuild into separate files is what keeps each as its own
-- single-statement, non-transactional execution.
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
        RAISE NOTICE 'dropped INVALID idx_campaigns_stuck_claims from a failed CONCURRENTLY build; 000015 will rebuild it';
    END IF;
END
$$;
