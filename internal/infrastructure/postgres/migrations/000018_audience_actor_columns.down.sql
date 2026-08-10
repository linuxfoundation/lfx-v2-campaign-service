-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the column discards the attribution it holds; there is no other copy.
ALTER TABLE campaign_audiences
    DROP COLUMN IF EXISTS updated_by;
