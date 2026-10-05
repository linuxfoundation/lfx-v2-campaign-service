-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops slot_version and its CHECK. It does NOT touch campaigns.version, which
-- is the unrelated optimistic-concurrency counter from 000002.
--
-- Run 000037's down first (golang-migrate does, descending): the four-column index reads
-- this column. Safe on its own while uq_campaigns_brief_platform_variant_live still exists,
-- because that index has kept every slot at one live row. Once a later migration drops it
-- and a slot holds a second live campaign, reverting this far would leave two live rows
-- the three-column index cannot be recreated over — choosing which one stays live is a data
-- decision, not a schema one, and is not automated here.

ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_slot_version_positive;
ALTER TABLE campaigns DROP COLUMN IF EXISTS slot_version;
