-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Drop the build lease. CONCURRENTLY for the same reason the build was: a plain DROP
-- INDEX takes a lock that blocks writes on campaign_audiences, and a rollback runs while
-- other replicas are still building. It also clears the INVALID index a failed concurrent
-- build leaves behind, so a retry of the up migration is clean.
--
-- Rolling back restores the old behaviour, in which two concurrent builds for one brief
-- each create a full set of HubSpot lists. There is nothing to restore in its place: the
-- lease replaced no earlier constraint.
DROP INDEX CONCURRENTLY IF EXISTS uq_campaign_audiences_brief_platform_building;
