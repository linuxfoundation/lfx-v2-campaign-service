-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops bidding_strategy and its CHECK. The recorded choices are lost; the ad
-- platforms still hold each campaign's real strategy, and they are authoritative.

ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_bidding_strategy_valid;
ALTER TABLE campaigns DROP COLUMN IF EXISTS bidding_strategy;
