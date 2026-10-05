-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- The slot index gains slot_version: at most one LIVE campaign per
-- (brief, platform, variant, slot_version).
--
-- This WIDENS the key, so it permits strictly more than its predecessor. That direction
-- matters for expand/contract: uq_campaigns_brief_platform_variant_live stays in place and
-- keeps serializing the N-1 binary's claims, which still name the three-column conflict
-- target. The old index is dropped a release later, once no running binary depends on it.
--
-- While both indexes exist, slot_version 2 CANNOT be created: the three-column index still
-- rejects it, and because this release's claim names the four-column index as its ON
-- CONFLICT arbiter, that rejection surfaces as a unique violation rather than being
-- swallowed. The repository classifies it as domain.ErrSlotVersionUnavailable so the job
-- reports why. That is the expand phase working as designed.
--
-- CONCURRENTLY, and alone in this file, for the reasons 000013 and 000022 set out: a
-- blocking build would stall in-flight claims during a rolling restart, and a
-- multi-statement file is batched into an implicit transaction, which CONCURRENTLY cannot
-- run inside.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_campaigns_brief_platform_variant_slot_version_live
    ON campaigns (brief_id, platform, variant, slot_version)
    WHERE status <> 'deleted';
