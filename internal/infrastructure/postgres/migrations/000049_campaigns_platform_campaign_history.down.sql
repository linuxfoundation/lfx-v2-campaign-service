-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Keep this the only statement: concurrent index operations cannot run in a transaction.
DROP INDEX CONCURRENTLY IF EXISTS idx_campaigns_platform_campaign_history;
