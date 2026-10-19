-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000047's survey stage pair: the two asset stages, without the series.
--
-- NARROWING, so it is only safe once no `Survey Reminder` or `Survey Closing` row exists: the ADD
-- CONSTRAINT below is validated against the whole table and FAILS if one does, leaving the table
-- with no pair CHECK at all for the rest of the transaction. That is the honest outcome -- a down
-- migration must not silently delete briefs to make itself apply -- but it means an operator
-- rolling back a release that shipped reminder or closing briefs has to archive or re-stage those
-- rows first.
--
-- The delivery-type CHECK is not touched here for the reason the up migration does not touch it:
-- 000047 owns it, and `survey` remains a valid surface after this rolls back.

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
