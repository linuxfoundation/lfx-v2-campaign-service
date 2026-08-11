-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- CONCURRENTLY here too: a plain DROP INDEX takes a lock that blocks writes to
-- campaigns, and a down migration runs against a live rolling deployment for the
-- same reason the up migration does.
DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_platform_campaign_live;
