-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add campaigns.max_cpc_bid: the manual max cost-per-click bid most recently SET through
-- update-campaign-bid (LFXV2-2665), in the ad account's own currency.
--
-- What it records, and what it does not. It is the bid lever's twin of budget_amount: a
-- REQUEST the ad platform confirmed, written only after that confirmation, never an
-- observation read back from the platform. NULL means "never set through this endpoint",
-- not "no bid" — a bid given at creation lives in config_snapshot (each platform's own
-- config key), and a bid changed in the platform's own UI is recorded nowhere here.
--
-- Why a typed column rather than patching config_snapshot. config_snapshot is the
-- dispatch config as each PLATFORM spelled it (Microsoft's cpcBid, for one); writing a
-- platform-specific key into it from the service layer would put platform knowledge in
-- the layer designed not to hold any, and would make the snapshot stop meaning "what the
-- dispatch asked for". A column states the request in this service's own vocabulary, as
-- budget_amount does.
--
-- NUMERIC(18,6), not budget_amount's (14,2). The bid contract's floor is one micro (the
-- unit Reddit bids in), and a CPC bid is routinely a fraction of a cent-scale amount in
-- some currencies, so a two-place scale would silently round what the platform was told.
-- Twelve integer digits hold the contract's 1,000,000 ceiling with room to spare.
--
-- EXPAND-only and nullable with no default: the N-1 binary does not know the column,
-- never writes it, and its UPDATE leaves it untouched, so a rolling deploy is safe in both
-- directions. Nothing is backfilled — there is no evidence of any earlier bid change to
-- record.

ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS max_cpc_bid NUMERIC(18,6);

-- A bid is strictly positive; the service refuses zero and negatives before the platform is
-- contacted, so this CHECK only catches a defect that slipped past it.
ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_max_cpc_bid_positive;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_max_cpc_bid_positive CHECK (max_cpc_bid IS NULL OR max_cpc_bid > 0);
