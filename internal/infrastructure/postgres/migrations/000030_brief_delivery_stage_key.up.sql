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
--
-- NOT `CONCURRENTLY`, deliberately, and the reason is worth stating because the repo checklist
-- otherwise reads as requiring it. Two independent reasons:
--
--   1. It is FORBIDDEN here. `CONCURRENTLY` may not run inside a transaction, and the checklist
--      in this directory's README requires it to live ALONE in its file for exactly that reason:
--      a multi-statement migration is batched in an implicit transaction. This file is
--      multi-statement (two ALTERs, a constraint swap, a DROP INDEX and a CREATE INDEX).
--   2. It is UNNECESSARY. The chart pins `strategy.type: Recreate` (deployment.yaml) with
--      `replicaCount: 1` (values.yaml), so the old pod is fully terminated before the new one
--      starts and migrates. There is no concurrent writer for a plain CREATE INDEX to block,
--      and no window in which old code meets the new schema -- which is also why the narrowing
--      this migration performs satisfies the README's expand/contract rule.
--
-- BOTH of those are load-bearing. Raising `replicaCount` above 1, or restoring `RollingUpdate`,
-- reintroduces an overlap window in which a pre-000030 pod reads `WHERE project_id=$1 AND
-- event_slug=$2` against a table now holding several briefs per event -- and it would match an
-- arbitrary member of that set. Revisit this migration if either changes.

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

-- Reject the identity COMBINATIONS the model says cannot exist, not merely the two values
-- independently. Validating each column alone still admits `(paid-marketing, 'Registration Push')`
-- and `(email, '')`: neither is a brief the product has -- paid has no series and an email send is
-- always some stage -- yet each occupies its own slot in the unique index above, so they are extra
-- live briefs for one event rather than harmless bad data. A free-text stage does the same, and
-- unbounded: `(email, 'Nonsense Stage')` is a slot too, and one no lookup can ever name, since
-- find-brief validates against this same list. All three were storable and were verified so.
--
-- Enforced HERE and not only at the HTTP edge because this is the invariant the KEY depends on,
-- and the key is the database's. The generated Goa validators cover HTTP callers; a migration, a
-- backfill, or a psql session answers to this constraint alone.
--
-- Keep this list in step with `emailstage.Names()`. `TestPublishedStageEnumMatchesEmailStageNames`
-- pins the API's copy to that function; a seventh stage needs a migration to reach this one.
ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_stage_pair_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_stage_pair_valid
    CHECK (
        (delivery_type = 'paid-marketing' AND stage = '')
        OR (delivery_type = 'email' AND stage IN (
            'CFP Launch', 'Schedule Announcement', 'Registration Push',
            'Discount Offer', 'Final Countdown', 'Post-Event'
        ))
    );

DROP INDEX IF EXISTS uq_campaign_briefs_project_event;

CREATE UNIQUE INDEX IF NOT EXISTS uq_campaign_briefs_project_event_delivery_stage
    ON campaign_briefs (project_id, event_slug, delivery_type, stage)
    WHERE status <> 'archived';
