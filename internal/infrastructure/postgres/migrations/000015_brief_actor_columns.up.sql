-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Actor attribution on campaign_briefs.
--
-- Campaigns run under SYSTEM accounts: the ad platform sees one shared LF-owned
-- identity, so it cannot tell us which person created or edited a brief. If this
-- service does not record it, that information exists nowhere.
--
-- connections (000001) and campaign_audiences (000005) already carry created_by /
-- updated_by JSONB; campaign_briefs carried only approved_by, which answers "who
-- signed off on this content" and not "who wrote it" or "who touched it last".
-- Same column type and shape as the existing tables, so marshalActor /
-- unmarshalActor apply unchanged.
--
-- Nullable by necessity, not by preference: every row that already exists predates
-- this migration and has no actor to backfill, and some writes are legitimately
-- system-initiated with no human behind them. NULL means "not recorded", never
-- "nobody".
--
-- The campaigns table gets the same treatment in a follow-up; it is a separate
-- migration because its write paths (dispatch claim, upsert, status toggle) are a
-- distinct change with distinct failure modes.

ALTER TABLE campaign_briefs
    ADD COLUMN IF NOT EXISTS created_by JSONB,
    ADD COLUMN IF NOT EXISTS updated_by JSONB;
