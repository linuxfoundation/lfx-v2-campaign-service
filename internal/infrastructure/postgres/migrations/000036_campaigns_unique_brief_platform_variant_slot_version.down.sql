-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the four-column slot index. Safe while uq_campaigns_brief_platform_variant_live
-- still exists (it is the narrower key and keeps enforcing one live row per three-column
-- slot), which is the state 000036 was applied into.
--
-- Rolling back the CODE as well as this index is required: this release's claim, upsert and
-- adopt name the four-column index as their ON CONFLICT arbiter, and without it every one of
-- them fails with "no unique or exclusion constraint matching the ON CONFLICT
-- specification" (loudly, never as a duplicate).

DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_brief_platform_variant_slot_version_live;
