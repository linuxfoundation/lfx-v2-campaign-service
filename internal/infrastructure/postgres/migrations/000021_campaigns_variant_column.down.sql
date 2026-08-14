-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverses 000021. Ordering matters: 000023's down re-creates the (brief_id,
-- platform) index and 000022's down drops the widened one, so by the time this
-- runs nothing indexes `variant` any more. Dropping it earlier would fail.
--
-- DESTRUCTIVE for multi-variant data: a brief holding both a Search and a Demand
-- Gen campaign has two live rows that collapse to the same (brief_id, platform)
-- pair once this column is gone, so 000023's down would fail to build its unique
-- index. That is the correct failure -- rolling back past this point requires
-- deciding which of the two campaigns to keep, and the row carries
-- platform_campaign_id for a campaign that may still be spending upstream.

ALTER TABLE campaigns DROP COLUMN IF EXISTS variant;
