-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore the full UNIQUE (brief_id, platform) constraint from 000002.
--
-- This can FAIL by design. Once campaigns have been soft-deleted, a brief may
-- legitimately hold both a deleted campaign and a live one for the same platform
-- -- which the partial index permits and the full constraint does not. The failed
-- ALTER is the intended signal that the rollback is unsafe on this data: the
-- rows must be reconciled (hard-delete the soft-deleted duplicates, after
-- confirming nothing still exists upstream for them) before the schema can go
-- back. Mirrors 000003's down migration, which fails the same way on a
-- slug-valued project_id.
--
-- golang-migrate runs down migrations in DESCENDING version order, so this runs
-- BEFORE 000013 drops the partial index -- the table is never left without
-- uniqueness on the pair.
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_brief_id_platform_key UNIQUE (brief_id, platform);
