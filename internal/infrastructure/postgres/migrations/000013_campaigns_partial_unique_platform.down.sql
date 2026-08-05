-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the partial unique index created by the up migration. CONCURRENTLY for the
-- same reason the build was: a plain DROP INDEX takes a lock that blocks writes on
-- campaigns, and a rollback runs while other replicas are still dispatching. Also
-- clears the INVALID index a failed concurrent build leaves behind, so a retry of
-- the up migration is clean.
--
-- 000011's down migration restores the full UNIQUE (brief_id, platform) constraint,
-- and golang-migrate runs down migrations in DESCENDING version order, so by the
-- time this runs the old constraint is already back in place -- the table is never
-- left without uniqueness on the pair.
DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_brief_platform_live;
