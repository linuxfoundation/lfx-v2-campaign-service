-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000052's survey stage pair: the two asset stages, without the series.
--
-- NARROWING, so it is only safe once no `Survey Reminder` or `Survey Closing` row exists: the ADD
-- CONSTRAINT below is validated against the whole table and FAILS if one does. This file is
-- multi-statement and so runs in one implicit transaction; that failure aborts it, rolling back
-- the DROP too, so the table keeps 000053's pair CHECK (golang-migrate records the version dirty
-- until it is cleared). That is the honest outcome -- a down migration must not silently delete
-- briefs to make itself apply.
--
-- What it means for an operator rolling back a release that shipped reminder or closing briefs:
-- those rows must first be DELETED, or moved to another stage, by SQL. Archiving does not help --
-- it changes only `status`, and the CHECK reads `stage` -- and neither does re-staging or the API,
-- because a brief's stage is identity that replace refuses to change. Moving a row's stage by SQL
-- can also collide with an existing brief in the same (event, delivery, stage) slot.
--
-- The delivery-type CHECK is not touched here for the reason the up migration does not touch it:
-- 000052 owns it, and `survey` remains a valid surface after this rolls back.

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
        OR (delivery_type = 'survey' AND stage IN (
            'Survey Promo', 'Report Promo'
        ))
    );
