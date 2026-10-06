-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Reverting drops max_cpc_bid and its CHECK. The recorded bids are lost; the platforms
-- still hold them, and they are authoritative, so nothing upstream changes.

ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_max_cpc_bid_positive;
ALTER TABLE campaigns DROP COLUMN IF EXISTS max_cpc_bid;
