-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Re-create 000022's three-column slot index exactly as 000022 defined it.
--
-- This FAILS if any slot holds more than one live campaign — a second campaign created with
-- new_version after 000040 — and that failure is correct: those rows cannot coexist under the
-- narrower key, and choosing which one stays live is a data decision, not a migration's. Each
-- carries a platform_campaign_id for a campaign that may still be spending. Soft-delete (or
-- otherwise resolve) the extra versions by hand, drop the INVALID index the failed build
-- leaves behind, and re-run.
--
-- NOT `IF NOT EXISTS`, for the reason 000024's down gives: a failed CONCURRENTLY build leaves
-- an INVALID index under this name, and IF NOT EXISTS would then silently skip the rebuild and
-- mark the rollback done with nothing enforcing the old key. A duplicate-name error on a
-- re-run is recoverable by hand; a silently missing index is not.
--
-- Rolling back the CODE as well is expected but not required for correctness: this release's
-- statements still name the four-column index as their arbiter, so with this index back a
-- claim above slot 1 on an occupied slot fails loudly (23505) instead of creating anything.
--
-- CONCURRENTLY, and alone in this file, for the reasons 000022 sets out.

CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_brief_platform_variant_live
    ON campaigns (brief_id, platform, variant)
    WHERE status <> 'deleted';
