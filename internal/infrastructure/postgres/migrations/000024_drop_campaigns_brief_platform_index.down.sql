-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Re-create the narrower index this migration dropped, BEFORE 000022's down
-- removes the widened one -- so the table is never left with no uniqueness on the
-- dispatch claim.
--
-- This FAILS if any brief holds two live campaigns on one platform (a Search and a
-- Demand Gen), and that failure is correct: those rows cannot coexist under the
-- narrower key, and choosing which to discard is not a migration's decision -- each
-- carries a platform_campaign_id for a campaign that may still be spending.

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_campaigns_brief_platform_live
    ON campaigns (brief_id, platform)
    WHERE status <> 'deleted';
