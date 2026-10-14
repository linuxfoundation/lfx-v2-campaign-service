-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- CONCURRENTLY for 000020's down-migration reason: a plain DROP INDEX blocks writes to
-- campaigns on a live rolling deployment.
DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_microsoft_campaign_live;
