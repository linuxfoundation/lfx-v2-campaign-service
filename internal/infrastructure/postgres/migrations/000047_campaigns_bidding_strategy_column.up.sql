-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Add campaigns.bidding_strategy: how the create path asked the ad platform to bid
-- (LFXV2-2665) — 'automatic' (the default when the dispatch config omits `bidding`) or
-- 'manual' (a manual max cost-per-click bid, whose starting amount is written to
-- max_cpc_bid in the same row write).
--
-- 000047, not 000043: 000042 is on main (#318), and PR #328
-- (fix/LFXV2-2665-adoption-binding-indexes) claims 000043-000046 (per-provider binding indexes).
-- THIS MIGRATION MUST APPLY AFTER 000043-000046. golang-migrate records the highest applied version
-- and never applies a lower one afterwards, so if 000047 reached a database first, 000043-000046
-- would be skipped forever. #328 merged first, so 000043-000046 precede this file on main;
-- TestMigration000047_RequiresItsPredecessorsEmbedded fails if the embedded migrations ever carry
-- 000047 without 000043-000046.
--
-- What it records. A REQUEST the create path sent, never an observation: an operator can
-- switch the strategy in the ad platform afterwards, and the bid lever reads the platform
-- before it writes, so this column never gates a write by itself. It exists so a read of the
-- campaign states which choice it was created with, in this service's own vocabulary rather
-- than each platform's (MaxClicks, LOWEST_COST_WITHOUT_CAP, BIDLESS, AUTO, MAX_CONVERSION).
--
-- EXPAND-only, nullable, no default, no backfill: the N-1 binary neither reads nor writes
-- the column (its upsert and update name their columns), so a rolling deploy is safe in both
-- directions. NULL means "not recorded" — a row created before this column, or an ADOPTED
-- campaign whose strategy this service never chose — and is never read as 'automatic'.

ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS bidding_strategy TEXT;

ALTER TABLE campaigns
    DROP CONSTRAINT IF EXISTS campaigns_bidding_strategy_valid;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_bidding_strategy_valid
    CHECK (bidding_strategy IS NULL OR bidding_strategy IN ('automatic', 'manual'));
