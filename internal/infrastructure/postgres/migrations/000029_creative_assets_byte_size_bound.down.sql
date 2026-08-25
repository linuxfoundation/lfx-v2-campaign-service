-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Dropping the bound WIDENS the table, so it is safe in the direction a rollback runs: every row
-- that satisfied the constraint still satisfies the column's remaining CHECK
-- (byte_size = octet_length(bytes)), and no reader depends on the upper bound holding.
--
-- What it does NOT restore is enforcement below the HTTP handler. After this runs, the 30 MiB
-- ceiling is once again guarded only by internal/service's len() check on the upload path, so a
-- repository caller or backfill can persist a larger blob -- the gap this migration closed.
ALTER TABLE creative_assets
    DROP CONSTRAINT IF EXISTS creative_assets_byte_size_max;
