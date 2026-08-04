-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the full UNIQUE (brief_id, platform) constraint from 000002, now that
-- 000014's partial unique index (excluding soft-deleted rows) enforces the same
-- rule for LIVE campaigns. Until this runs, the old constraint still covers
-- deleted rows, so a deleted campaign's slot stays occupied and the whole point of
-- 000014 is unrealized.
--
-- Why this is a SEPARATE migration from 000014 rather than a second statement
-- there: 000014's CREATE INDEX CONCURRENTLY cannot share a file with other
-- statements (a multi-statement migration is batched, which reintroduces the
-- implicit transaction that CONCURRENTLY forbids). Splitting also gives the
-- required ordering for free -- golang-migrate applies versions in ascending
-- order, so the replacement index always exists before the constraint it replaces
-- is removed. There is therefore no window in which the pair has no uniqueness at
-- all, which matters because that window is exactly when two concurrent
-- ClaimCampaignDispatch calls could both win and double-create a paid campaign
-- upstream.
--
-- The guard. A failed CREATE INDEX CONCURRENTLY does not roll back -- it leaves an
-- INVALID index that Postgres refuses to use, and 000014's IF NOT EXISTS would
-- then skip the rebuild while reporting success. If this migration dropped the old
-- constraint anyway, campaigns would be left with NO enforceable uniqueness on
-- (brief_id, platform): every dispatch claim would win, and concurrent retries
-- would create duplicate paid campaigns upstream -- silently, since nothing would
-- error. So the drop is conditional on the replacement index being present AND
-- indisvalid, and RAISEs otherwise. Failing the migration (and so the pod's
-- startup) is the correct outcome: it is loud, it is recoverable by re-running
-- 000014's build, and it leaves the old constraint protecting the table meanwhile.
--
-- The DO block also means this migration is NOT idempotent-by-accident: on a
-- database where the constraint was already dropped, the IF EXISTS check simply
-- finds nothing to do and the guard is not consulted.
--
-- Object names are schema-qualified. Unqualified names resolve through
-- search_path, which is fine today (single schema) but would let a future
-- multi-schema setup inspect one index and drop another table's constraint.
DO $$
BEGIN
    -- Nothing to drop: the constraint is already gone (re-run, or a database
    -- provisioned after this migration). Do not consult the guard.
    IF NOT EXISTS (
        SELECT 1
        FROM pg_constraint
        WHERE conname = 'campaigns_brief_id_platform_key'
          AND conrelid = 'public.campaigns'::regclass
    ) THEN
        RETURN;
    END IF;

    IF NOT EXISTS (
        SELECT 1
        FROM pg_class c
        JOIN pg_index i ON i.indexrelid = c.oid
        WHERE c.relname = 'uq_campaigns_brief_platform_live'
          AND c.relnamespace = 'public'::regnamespace
          AND i.indisvalid
    ) THEN
        RAISE EXCEPTION
            'refusing to drop campaigns_brief_id_platform_key: the replacement index uq_campaigns_brief_platform_live is missing or INVALID (a failed CONCURRENTLY build in 000014); dropping the constraint now would leave (brief_id, platform) with no uniqueness, allowing duplicate paid campaigns. Rebuild the index, then re-run this migration.';
    END IF;

    EXECUTE 'ALTER TABLE public.campaigns DROP CONSTRAINT campaigns_brief_id_platform_key';
END
$$;
