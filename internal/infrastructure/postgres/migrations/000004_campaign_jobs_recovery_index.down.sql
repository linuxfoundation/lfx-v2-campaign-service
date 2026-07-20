-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the partial recovery index added in 000004.up.sql.
DROP INDEX IF EXISTS idx_campaign_jobs_recovery;
