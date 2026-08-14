-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverses 000022. Runs AFTER 000023's down has re-created the narrower
-- (brief_id, platform) index, so the table is never left with no working
-- uniqueness on the dispatch claim -- which would allow a duplicate paid campaign.

DROP INDEX CONCURRENTLY IF EXISTS uq_campaigns_brief_platform_variant_live;
