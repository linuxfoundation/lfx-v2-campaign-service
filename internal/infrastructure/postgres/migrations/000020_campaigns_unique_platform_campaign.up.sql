-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Stop one upstream campaign from being bound to two briefs at once.
--
-- 000013's partial index keys (brief_id, platform), which answers "does this
-- brief already have a live campaign here" and nothing else. Adoption asks the
-- opposite question: the caller names an ARBITRARY upstream campaign, so two
-- different briefs in the same project can each adopt the SAME one. Both rows
-- then look provisioned, and the toggle and metrics reader on each act on the
-- same paid campaign independently -- one brief pauses what the other just
-- enabled, and neither operator can see why. Nothing in the service can detect
-- that after the fact: the two rows are individually well-formed.
--
-- Scoped by project_id, not global. A bare platform_campaign_id is unique only
-- within the customer it was created under (see the account-mismatch guards in
-- internal/dispatch/googleads.go, which exist precisely because of that), and a
-- project's connection pins one account. Keying globally would reject a
-- legitimate adoption in project B because project A's DIFFERENT account happens
-- to mint the same numeric id.
--
-- platform_campaign_id IS NOT NULL keeps the index off rows that have no upstream
-- campaign yet -- a dispatch claim inserts before the platform mints an id, and
-- those rows are not bindings of anything.
--
-- status <> 'deleted' for the same reason as 000013: soft delete must free the
-- slot, or a campaign deleted here could never be adopted again.
--
-- CONCURRENTLY, one statement per file, and no transaction -- see 000013 for why
-- all three are required by this runner. A failed build leaves the index INVALID
-- rather than rolling back; the deploy fails loudly instead of silently running
-- without the guard.
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS uq_campaigns_project_platform_campaign_live
    ON campaigns (project_id, platform, platform_campaign_id)
    WHERE status <> 'deleted' AND platform_campaign_id IS NOT NULL;
