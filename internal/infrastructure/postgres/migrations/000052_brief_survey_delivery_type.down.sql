-- Copyright The Linux Foundation and each contributor to LFX.
-- SPDX-License-Identifier: MIT

-- Restore the two-surface CHECKs from 000030.
--
-- NARROWING, so it is only safe once no `survey` row exists: the ADD CONSTRAINT below is validated
-- against the whole table and FAILS if one does, leaving the table with no delivery-type CHECK at
-- all for the rest of the transaction. That is the honest outcome -- a down migration must not
-- silently delete briefs to make itself apply -- but it means an operator rolling back a release
-- that shipped survey briefs has to archive or re-type those rows first.
--
-- The DROPs run before the ADDs so a partially-applied up migration still rolls back cleanly.

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_stage_pair_valid;

ALTER TABLE campaign_briefs
    DROP CONSTRAINT IF EXISTS campaign_briefs_delivery_type_valid;

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_type_valid
    CHECK (delivery_type IN ('paid-marketing', 'email'));

ALTER TABLE campaign_briefs
    ADD CONSTRAINT campaign_briefs_delivery_stage_pair_valid
    CHECK (
        (delivery_type = 'paid-marketing' AND stage = '')
        OR (delivery_type = 'email' AND stage IN (
            'CFP Launch', 'Schedule Announcement', 'Registration Push',
            'Discount Offer', 'Final Countdown', 'Post-Event'
        ))
    );
