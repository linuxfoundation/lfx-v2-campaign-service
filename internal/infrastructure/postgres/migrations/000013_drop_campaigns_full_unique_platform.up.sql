-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the full UNIQUE (brief_id, platform) constraint from 000002, now that
-- 000012's partial unique index (excluding soft-deleted rows) enforces the same
-- rule for LIVE campaigns. Until this runs, the old constraint still covers
-- deleted rows, so a deleted campaign's slot stays occupied and the whole point of
-- 000012 is unrealized.
--
-- Why this is a SEPARATE migration from 000012 rather than a second statement
-- there: 000012's CREATE INDEX CONCURRENTLY cannot share a file with other
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
-- INVALID index that Postgres refuses to use, and 000012's IF NOT EXISTS would
-- then skip the rebuild while reporting success. If this migration dropped the old
-- constraint anyway, campaigns would be left with NO enforceable uniqueness on
-- (brief_id, platform): every dispatch claim would win, and concurrent retries
-- would create duplicate paid campaigns upstream -- silently, since nothing would
-- error. So the drop is conditional on the replacement index being present, valid,
-- AND matching its required definition (see the guard below -- a name check alone is
-- not enough, because 000012's IF NOT EXISTS lets a pre-existing index of the same
-- name suppress the real build), and RAISEs otherwise. Failing the migration (and so
-- the pod's startup) is the correct outcome: it is loud, it is recoverable by
-- re-running 000012's build, and it leaves the old constraint protecting the table
-- meanwhile.
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

    -- Check the index's DEFINITION, not just its name. A name-and-indisvalid check is
    -- not enough: 000012 builds with IF NOT EXISTS, so any pre-existing index that
    -- happens to carry this name makes 000012 a silent no-op AND satisfies a name-only
    -- guard -- and then this migration drops the sole real uniqueness constraint. The
    -- index must therefore be proven to enforce what the constraint enforced:
    --   * on public.campaigns (not some other table's index of the same name),
    --   * UNIQUE (indisunique) -- a non-unique index arbitrates no dispatch claim,
    --   * keyed on exactly (brief_id, platform), in that order, as its only two key
    --     columns -- indnkeyatts pins the count so a superset like
    --     (brief_id, platform, id) cannot pass, since it would permit two live rows
    --     for the pair,
    --   * partial on precisely `status <> 'deleted'` -- a different predicate covers a
    --     different set of rows. The predicate is compared as the deparsed text
    --     Postgres itself produces, against the one form it deparses this expression
    --     to; a partial index whose WHERE clause differs at all fails the guard, which
    --     is the safe direction (a false alarm is loud and recoverable, a false pass
    --     silently removes the last uniqueness).
    IF NOT EXISTS (
        SELECT 1
        FROM pg_index i
        JOIN pg_class c ON c.oid = i.indexrelid
        WHERE c.relname = 'uq_campaigns_brief_platform_live'
          AND c.relnamespace = 'public'::regnamespace
          AND i.indrelid = 'public.campaigns'::regclass
          AND i.indisvalid
          AND i.indisunique
          AND i.indnkeyatts = 2
          AND (SELECT a.attname FROM pg_attribute a
                WHERE a.attrelid = i.indrelid AND a.attnum = i.indkey[0]) = 'brief_id'
          AND (SELECT a.attname FROM pg_attribute a
                WHERE a.attrelid = i.indrelid AND a.attnum = i.indkey[1]) = 'platform'
          AND i.indpred IS NOT NULL
          AND pg_get_expr(i.indpred, i.indrelid)
              = '(status <> ''deleted''::text)'
    ) THEN
        RAISE EXCEPTION
            'refusing to drop campaigns_brief_id_platform_key: the replacement index public.uq_campaigns_brief_platform_live is missing, INVALID (a failed CONCURRENTLY build in 000012), or does not match its required definition -- UNIQUE on public.campaigns, keyed on exactly (brief_id, platform), partial WHERE status <> ''deleted''. Dropping the constraint now would leave (brief_id, platform) with no enforceable uniqueness, allowing duplicate paid campaigns. Inspect the index, rebuild it from 000012, then re-run this migration.';
    END IF;

    EXECUTE 'ALTER TABLE public.campaigns DROP CONSTRAINT campaigns_brief_id_platform_key';
END
$$;
