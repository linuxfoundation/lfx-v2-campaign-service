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

-- NOT `IF NOT EXISTS`. A CONCURRENTLY build that fails leaves an INVALID index behind
-- under the same name, and `IF NOT EXISTS` would then silently skip the rebuild: this
-- migration would be marked down, 000022's down would drop the widened arbiter, and the
-- table would be left with NO working uniqueness on the dispatch claim -- nothing stopping
-- a retry creating a second paid campaign.
--
-- The forward path has 000023 to catch exactly that condition. The rollback path has no
-- later guard, so failing loudly here is the only place it can be caught. A duplicate-name
-- error on rollback is recoverable by hand (drop the invalid index, re-run); a silently
-- missing arbiter is not, because nothing reports it.
CREATE UNIQUE INDEX CONCURRENTLY uq_campaigns_brief_platform_live
    ON campaigns (brief_id, platform)
    WHERE status <> 'deleted';
