-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Widen brief identity from (project, event) to (project, event, delivery_type, stage).
--
-- 000003 made `(project_id, event_slug)` unique among non-archived rows, which encodes "one
-- brief per event". That was true when a brief meant a paid-marketing plan. It is not true of
-- the product:
--
--   1. Paid and email are PARALLEL channels. One event is promoted on both at once, and
--      neither should be able to displace the other. Under the old key the second channel to
--      save either overwrote the first or was refused; both are wrong.
--   2. An email campaign is a SERIES, not a document. One event yields a CFP Launch, a Schedule
--      Announcement, a Registration Push, a Discount Offer, a Final Countdown and a Post-Event
--      send -- six briefs for one event, all live at once. `CampaignEmailStage` already
--      enumerates them and `email-copy` already generates per stage; only storage disagreed.
--
-- So the key gains the two dimensions that actually distinguish one brief from another.
--
-- BACKFILL IS THE IDENTITY OF EXISTING ROWS, not a default for convenience. Every row written
-- before this migration came from the paid surface -- it was the only one whose brief could be
-- saved -- so `delivery_type` backfills to 'paid-marketing' and that is a statement of fact
-- about those rows, not a fallback. `stage` backfills to '' rather than NULL because NULL is
-- not comparable in a unique index: two NULL stages do not conflict, so a NULL-bearing key
-- would let unlimited duplicate paid briefs accumulate for one event, silently undoing 000003.
-- The empty string is the real stage of a paid brief -- paid has no series -- and it compares.
--
-- Partial on `status <> 'archived'` exactly as 000003 was, so archiving still frees the slot.
-- The old index is dropped rather than left alongside: keeping it would enforce the very
-- constraint this migration exists to lift.

ALTER TABLE campaign_briefs
    ADD COLUMN IF NOT EXISTS delivery_type TEXT NOT NULL DEFAULT 'paid-marketing';

ALTER TABLE campaign_briefs
    ADD COLUMN IF NOT EXISTS stage TEXT NOT NULL DEFAULT '';

-- Reject a delivery type the service cannot act on. Enumerated rather than free text because
-- the value selects which generators run and which surface may open the row; an unrecognised
-- one would be silently treated as paid by every reader.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email'));

DROP INDEX IF EXISTS uq_campaign_briefs_project_event;

CREATE UNIQUE INDEX IF NOT EXISTS uq_campaign_briefs_project_event_delivery_stage
    ON campaign_briefs (project_id, event_slug, delivery_type, stage)
    WHERE status <> 'archived';
