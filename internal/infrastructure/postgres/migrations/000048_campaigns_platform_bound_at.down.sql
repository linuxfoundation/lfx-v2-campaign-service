-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the column returns the deleted-campaign scope's ownership order to created_at, the
-- order an N-1 binary already uses. Nothing else reads it, and the stamps it loses describe only
-- that ordering.
ALTER TABLE campaigns
    DROP COLUMN IF EXISTS platform_bound_at;
