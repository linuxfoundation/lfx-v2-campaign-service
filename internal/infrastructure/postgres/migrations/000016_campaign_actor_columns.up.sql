-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Actor attribution on campaigns. The follow-up 000015 forecast.
--
-- Campaigns run under SYSTEM accounts: the ad platform sees one shared LF-owned
-- identity, so it cannot tell us which person launched or edited a campaign. If this
-- service does not record it, that information exists nowhere. campaigns is the table
-- where that matters most — these rows are what SPEND MONEY.
--
-- Same column type and shape as connections (000001), campaign_audiences (000005) and
-- campaign_briefs (000015), so marshalActor / unmarshalActor apply unchanged.
--
-- Why this is a SEPARATE migration from 000015 rather than the same one: the write
-- paths differ in kind, not just in table. A brief write happens ON the request
-- goroutine, so the actor is read from the request context at the point of the write.
-- Campaign creation does not: dispatch runs on the orchestrator's ROOT context, in a
-- goroutine that outlives the request, so `actorFromCtx` at the point of the INSERT
-- would return nil for every campaign ever created. The actor has to be captured at
-- Orchestrator.Start, while the request context is still in hand, and threaded down
-- through run → dispatchPlatform → ClaimCampaignDispatch/UpsertCampaign. That is a
-- signature change across the dispatch path, and it deserved its own review.
--
-- What is captured is the DECODED actor value (name, email, principal), never the
-- bearer token. A token captured for asynchronous use may be expired by the time the
-- work runs and there is no retry; a decoded value has no expiry and is the exact
-- thing being recorded.
--
-- Nullable by necessity, not by preference: every row that already exists predates
-- this migration and has no actor to backfill, and some writes are legitimately
-- system-initiated with no human behind them — notably the recovery sweeper, which
-- has no originating request at all. NULL means "not recorded", never "nobody".
--
-- These columns hold personal data (name, email). Nothing prunes them: the record
-- lives as long as the campaign, because an audit trail that expires answers "who
-- spent this money" only for recent writes. Adding a deletion path is a compliance
-- decision.
--
-- campaign_audiences still carries created_by ONLY, so an audience edit records no
-- actor. That remains open (LFXV2-3038 follow-up); it is not closed here because
-- update-audience is a published PATCH with its own handler and its own tests.

ALTER TABLE campaigns
    ADD COLUMN IF NOT EXISTS created_by JSONB,
    ADD COLUMN IF NOT EXISTS updated_by JSONB;
