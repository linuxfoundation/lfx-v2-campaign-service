-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the columns DISCARDS every recorded dimension. Unlike 000028's down migration, nothing
-- unrecoverable is lost: width and height are derived from bytes, which stays, so a re-migration
-- plus the backfill recovers them exactly. What a rollback DOES lose until that backfill runs is
-- every asset's classification -- the slot planner has no shape to match against and reports the
-- whole library as unclassified rather than mis-classifying it.
--
-- The constraint is dropped first and explicitly rather than being left to the column drop's
-- cascade, so a partially-applied up migration (columns added, constraint not) still rolls back
-- cleanly; IF EXISTS makes both halves idempotent.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_dimensions_positive;

ALTER TABLE creative_assets
    DROP COLUMN IF EXISTS width,
    DROP COLUMN IF EXISTS height;
