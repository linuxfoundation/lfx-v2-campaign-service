-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore 000053's checks, without the newsletter surface.
--
-- NARROWING, so it is only safe once no `newsletter` brief exists: each ADD CONSTRAINT below is
-- validated against the whole table and FAILS if one does. The file runs in one implicit
-- transaction, so that failure rolls back the DROPs too and the table keeps 000059's checks. A
-- down migration must not silently delete briefs to make itself apply: an operator rolling back a
-- release that shipped newsletter briefs deletes those rows by SQL first.

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
            'Survey Promo', 'Survey Reminder', 'Survey Closing', 'Report Promo'
        ))
    );

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email', 'survey'));
