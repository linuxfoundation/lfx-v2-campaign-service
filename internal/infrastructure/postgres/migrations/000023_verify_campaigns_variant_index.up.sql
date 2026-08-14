-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- VERIFY that 000022's replacement index is real before 000024 drops the narrower
-- (brief_id, platform) one. This migration only CHECKS -- it changes nothing.
--
-- The check and the drop are separate migrations because DROP INDEX CONCURRENTLY
-- cannot run inside a transaction, and a DO $$ block IS one. 000014 could do both
-- at once only because it drops a CONSTRAINT, which is transaction-safe. So the
-- guard runs here and raises, and 000024 does the bare CONCURRENTLY drop that
-- golang-migrate will only reach if this one succeeded.
--
-- GUARDED, mirroring 000014. The check is on the new index's DEFINITION, not just
-- its name, and the reason is specific: 000022 builds with IF NOT EXISTS, so any
-- pre-existing index carrying that name would make 000022 a silent no-op AND
-- satisfy a name-only guard -- after which this migration would drop the sole
-- index arbitrating the dispatch claim. A failed CONCURRENTLY build is the same
-- hazard from the other direction: it leaves the index present but INVALID, and
-- an invalid index enforces nothing.
--
-- So the replacement must be proven to enforce what the old one enforced:
--   * on public.campaigns, not another table's index of the same name
--   * UNIQUE -- a non-unique index arbitrates no claim
--   * VALID and READY -- indisvalid AND indisready. The two fail APART: a
--     CONCURRENTLY build that dies between its phases can leave an index marked
--     valid but not ready, and a not-ready index enforces nothing on new writes.
--     Checking only indisvalid would therefore let this guard pass on an index
--     that arbitrates no claim -- after which 000024 drops the real arbiter and
--     nothing stops a retry creating a second paid campaign. This is the same
--     pair the boot-time check requires (pool.go's requiredIndexQuery); the two
--     must agree, or a schema that satisfies the migration fails at startup.
--   * keyed on exactly (brief_id, platform, variant), in that order
--   * PARTIAL on status <> 'deleted', or a deleted campaign would never free its
--     slot
--
-- Failing loudly here is the correct outcome: the alternative is a table with no
-- working uniqueness on the claim, which is what stops a retry from creating a
-- second paid campaign upstream.

DO $$
DECLARE
    idx_oid oid;
BEGIN
    -- Nothing to drop: already gone (re-run, or a database provisioned after this
    -- migration). Do not consult the guard.
    IF NOT EXISTS (
        SELECT 1 FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = 'uq_campaigns_brief_platform_live'
          AND n.nspname = 'public'
          AND c.relkind = 'i'
    ) THEN
        RETURN;
    END IF;

    SELECT i.indexrelid INTO idx_oid
    FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relname = 'uq_campaigns_brief_platform_variant_live'
      AND n.nspname = 'public'
      AND i.indrelid = 'public.campaigns'::regclass
      AND i.indisunique
      AND i.indisvalid
      AND i.indisready
      AND i.indnkeyatts = 3
      AND pg_get_indexdef(i.indexrelid) LIKE '%(brief_id, platform, variant)%'
      -- The EXACT predicate, not merely that one exists. An impostor such as
      -- `WHERE status = 'created'` is unique, valid and correctly keyed, and would pass a
      -- non-null check while enforcing a different invariant entirely — after which 000024
      -- drops the real arbiter. Character-identical to the deparsed form, matching how
      -- 000014 compares its own.
      AND pg_get_expr(i.indpred, i.indrelid) = '(status <> ''deleted''::text)';

    IF idx_oid IS NULL THEN
        RAISE EXCEPTION
            'refusing to drop uq_campaigns_brief_platform_live: the replacement uq_campaigns_brief_platform_variant_live is missing, invalid, non-unique, not keyed on (brief_id, platform, variant), or not partial. Dropping it would leave the dispatch claim with no uniqueness to arbitrate on, allowing a duplicate paid campaign. Re-run 000022 and verify the index is VALID.';
    END IF;

END
$$;
